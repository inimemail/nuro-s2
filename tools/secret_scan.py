#!/usr/bin/env python3
"""Offline, bounded scan of Git-managed files. Never prints matched secrets."""
import argparse
import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
import re
import subprocess
import sys

MAX_BYTES = 8 * 1024 * 1024
RULES = {
    "private-key": re.compile(rb"-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----"),
    "github-token": re.compile(rb"\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{60,255})\b"),
    "provider-token": re.compile(rb"\bsk-(?:ant-api\d{2}-)?[A-Za-z0-9_-]{40,255}\b"),
    "aws-access-key": re.compile(rb"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b"),
}


@dataclass(frozen=True)
class Finding:
    rule: str
    path: str
    line: int


def scan_bytes(path, content):
    if b"\0" in content[:4096]:
        return []
    if len(content) > MAX_BYTES:
        return [Finding("text-file-too-large", path, 1)]
    findings = []
    for line_number, line in enumerate(content.splitlines(), 1):
        for rule, pattern in RULES.items():
            if not pattern.search(line):
                continue
            # A narrowly scoped inline waiver must name the rule and a reason.
            waiver = re.search(rb"secret-scan: allow " + rule.encode() + rb" -- (.{12,})", line)
            if waiver is None:
                findings.append(Finding(rule, path, line_number))
    return findings


def git_paths(root, changed_base=None):
    if changed_base:
        subprocess.run(["git", "rev-parse", "--verify", changed_base + "^{commit}"], cwd=root, check=True, capture_output=True)
        command = ["git", "diff", "--name-only", "--diff-filter=ACMR", "-z", changed_base, "--"]
    else:
        command = ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"]
    return sorted(set(p for p in subprocess.check_output(command, cwd=root).split(b"\0") if p))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--changed-base", help="Scan files changed since an existing commit; never evaluates shell input")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    try:
        allow_path = root / ".github/secret-scan-allowlist.json"
        allowed = json.loads(allow_path.read_text()) if allow_path.exists() else []
        if not isinstance(allowed, list) or any(
            not isinstance(entry, dict) or entry.get("rule") not in RULES
            or not isinstance(entry.get("reason"), str) or len(entry["reason"]) < 12
            or not re.fullmatch(r"[0-9a-f]{64}", str(entry.get("line_sha256", "")))
            or not isinstance(entry.get("path"), str) or Path(entry["path"]).is_absolute()
            or ".." in Path(entry["path"]).parts for entry in allowed
        ):
            raise ValueError("invalid narrow allowlist")
        allowed_lines = {(e["path"], e["rule"], e["line_sha256"]) for e in allowed}
        paths = git_paths(root, args.changed_base)
        findings = []
        for encoded in paths:
            name = encoded.decode("utf-8", errors="surrogateescape")
            relative = Path(name)
            if relative.is_absolute() or ".." in relative.parts:
                raise ValueError("unsafe Git path")
            path = root / relative
            if path.is_symlink() or not path.is_file():
                continue
            if not path.resolve().is_relative_to(root.resolve()):
                raise ValueError("Git path escapes repository")
            with path.open("rb") as handle:
                content = handle.read(MAX_BYTES + 1)
            lines = content.splitlines()
            for finding in scan_bytes(name, content):
                digest = hashlib.sha256(lines[finding.line - 1]).hexdigest()
                if (name, finding.rule, digest) not in allowed_lines:
                    findings.append(finding)
    except (OSError, ValueError, subprocess.CalledProcessError):
        print("Secret scan failed to read repository inputs", file=sys.stderr)
        return 2
    for finding in findings:
        # repr escapes hostile filename control characters; never expose content.
        print(f"{finding.rule}: {finding.path!r}:{finding.line}")
    if findings:
        print(f"FAIL: {len(findings)} high-confidence findings; matched values redacted")
        return 1
    print(f"PASS: {len(paths)} repository files checked offline; no high-confidence findings")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
