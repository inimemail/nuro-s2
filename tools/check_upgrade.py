#!/usr/bin/env python3
"""Run upgrade gates without changing toolchains, protected baselines or services.

Use GOTOOLCHAIN=go1.27.3 make check-upgrade. Integration requires Docker and
NURO_CACHE_TEST_DSN pointing at an isolated PostgreSQL fixture. --only can run a
stage during development; its report deliberately never claims full validation.
"""
from __future__ import annotations

import argparse
from contextlib import ExitStack
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
REQUIRED_TESTS = (
    "TestGeminiCachePostgresTransactions", "TestScopedAdminPostgresTransactions",
    "TestUpgradeV0219MigrationContracts", "TestConcurrencyCacheSuite/TestGetAccountsLoadBatch",
    "TestEdgeStreamRealRustTransportRecovery", "TestEdgeWSRealRustHealthSettlement",
    "TestEdgeWSRealRustFailedTurnReportsBeforeClientClose",
    "TestRegistrationBlacklistTransactions", "TestRegistrationBlacklistPreviewPagination",
    "TestRegistrationBlacklistAuditFailureAndConcurrentIdempotency",
    "TestUsageExportPostgresKeysetBoundaryAndFilters",
    "TestImageTaskPostgresBudgetAdmissionBeforeClaim",
)


def rust_version_supported(actual: str, minimum: str) -> bool:
    match = re.match(r"rustc (\d+)\.(\d+)\.(\d+)(?:\s|$)", actual)
    return bool(match and tuple(map(int, match.groups())) >= tuple(map(int, minimum.split("."))))


def go_test_counts(log: str) -> dict[str, int]:
    counts = {"pass": 0, "fail": 0, "skip": 0}
    for line in log.splitlines():
        try:
            item = json.loads(line)
        except ValueError:
            continue
        if isinstance(item, dict) and item.get("Test") and item.get("Action") in counts:
            counts[item["Action"]] += 1
    return counts


def preflight() -> bool:
    # Match repository declarations, without downloading or changing global defaults.
    expected_go = next(line.split()[1] for line in (ROOT/"backend/go.mod").read_text().splitlines() if line.startswith("go "))
    frontend = json.loads((ROOT/"frontend/package.json").read_text())
    versions = [(["go", "env", "GOVERSION"], "go"+expected_go, ROOT/"backend"),
                (["node", "--version"], "v"+frontend["engines"]["node"], ROOT/"frontend"),
                (["corepack", "pnpm", "--version"], frontend["engines"]["pnpm"], ROOT/"frontend")]
    for command, expected, cwd in versions:
        try:
            actual = subprocess.check_output(command, cwd=cwd, text=True).strip()
        except (OSError, subprocess.CalledProcessError):
            return False
        if actual != expected:
            print(f"Toolchain mismatch: {command[0]} expected {expected}, got {actual}", file=sys.stderr)
            return False
    # CI is the compatibility baseline; local newer Rust is allowed.
    workflow = (ROOT/".github/workflows/backend-ci.yml").read_text()
    minimum = re.search(r'dtolnay/rust-toolchain@(\d+\.\d+\.\d+)', workflow)
    try:
        actual_rust = subprocess.check_output(["rustc", "--version"], text=True).strip()
    except (OSError, subprocess.CalledProcessError):
        return False
    if not minimum or not rust_version_supported(actual_rust, minimum[1]):
        print(f"Rust toolchain mismatch: {actual_rust}; Cargo MSRV required", file=sys.stderr)
        return False
    return True


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--only", choices=("contracts", "go", "race", "integration", "frontend", "rust", "deploy", "security"))
    parser.add_argument("--log-dir", type=Path)
    args = parser.parse_args()
    logdir = args.log_dir or Path(tempfile.mkdtemp(prefix="nuro-upgrade-check-"))
    logdir.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(logdir, 0o700)
    results: list[dict] = []

    def run(name: str, command: list[str], cwd: Path = ROOT, env: dict | None = None, accepted_codes: tuple[int, ...] = (0,), separate_stderr: bool = False) -> bool:
        print(f"RUN {name}", flush=True)
        logfile = logdir / f"{name}.log"
        start = time.monotonic()
        stderrfile = logdir / f"{name}.stderr.log"
        with ExitStack() as files:
            output = files.enter_context(logfile.open("w", encoding="utf-8"))
            os.chmod(logfile, 0o600)
            stderr = subprocess.STDOUT
            if separate_stderr:
                stderr = files.enter_context(stderrfile.open("w", encoding="utf-8"))
                os.chmod(stderrfile, 0o600)
            try:
                proc = subprocess.run(command, cwd=cwd, env=env, stdout=output, stderr=stderr)
                code = proc.returncode
            except OSError as exc:
                output.write(str(exc))
                code = 127
        row = {"name": name, "command": command, "exit_code": code, "seconds": round(time.monotonic()-start, 2), "log": str(logfile)}
        if separate_stderr:
            row["stderr_log"] = str(stderrfile)
        if command[:3] == ["go", "test", "-json"]:
            row["tests"] = go_test_counts(logfile.read_text(errors="replace"))
        if accepted_codes != (0,):
            row["accepted_exit_codes"] = accepted_codes
            row["status"] = "CAPTURED" if code in accepted_codes else "FAIL"
        results.append(row)
        label = row.get("status", "PASS" if code == 0 else "FAIL")
        print(f"{label} {name}: {logfile}", flush=True)
        return code in accepted_codes


    selected = lambda stage: args.only is None or args.only == stage
    ok = preflight()
    if not ok:
        results.append({"name": "toolchain-preflight", "exit_code": 1})
    else:
        if selected("contracts"):
            ok &= run("protected", ["python3", "tools/check_upgrade_contracts.py"])
            ok &= run("tool-tests", ["python3", "-m", "unittest", "discover", "-s", "tools", "-p", "test_*.py"])
            ok &= run("diff", ["git", "diff", "--check"])
        if selected("go"):
            for label, extra in (("default", []), ("unit", ["-tags=unit"])):
                ok &= run("go-"+label, ["go", "test", "-json", *extra, "./..."], ROOT/"backend")
            ok &= run("go-build", ["go", "build", "./..."], ROOT/"backend")
            ok &= run("go-vet", ["go", "vet", "./..."], ROOT/"backend")
        if selected("race"):
            ok &= run("go-race", ["go", "test", "-json", "-race", "-tags=unit", "./internal/service/...", "./internal/repository/...", "./internal/pkg/..."], ROOT/"backend")
        if selected("rust"):
            ok &= run("rust", ["make", "test-rust"])
        if selected("integration"):
            ready = run("docker-preflight", ["docker", "info"])
            if not os.environ.get("NURO_CACHE_TEST_DSN"):
                results.append({"name": "isolated-postgres-preflight", "exit_code": 1, "reason": "NURO_CACHE_TEST_DSN is required; no production fallback"})
                ready = False
            if ready:
                ready &= run("edge-fixture", ["cargo", "build", "--manifest-path", "edge-rs/Cargo.toml", "--release", "--locked"])
            if ready:
                integration_env = dict(os.environ, NURO_EDGE_TEST_BINARY=str(ROOT/"edge-rs/target/release/sub2api-edge-rs"))
                ok &= run("go-integration", ["go", "test", "-json", "-tags=integration", "./..."], ROOT/"backend", integration_env)
                cmd = ["python3", "tools/check_go_test_results.py", str(logdir/"go-integration.log")]
                for test in REQUIRED_TESTS:
                    cmd.extend(["--require-test", test])
                ok &= run("integration-required", cmd)
            else:
                ok = False
        if selected("frontend"):
            ok &= run("frontend", ["make", "test-frontend-full"])
        if selected("deploy"):
            for script in ("deploy-nuro.sh", "deploy/single-host-autoscaler.sh", "deploy/install-datamanagementd.sh"):
                ok &= run("syntax-"+Path(script).name, ["bash", "-n", script])
            ok &= run("deploy-credentials", ["bash", "tools/test_deploy_admin_bootstrap.sh"])
            if (ROOT/"datamanagement/go.mod").is_file():
                ok &= run("datamanagement", ["make", "test-datamanagementd"])
            else:
                results.append({"name": "datamanagement", "status": "NOT_APPLICABLE", "reason": "Optional component source is absent from this repository; no test pass claimed"})
        if selected("security"):
            ok &= run("secret-scan", ["python3", "tools/secret_scan.py"])
            ok &= run("golangci", ["golangci-lint", "run", "./..."], ROOT/"backend")
            ok &= run("govulncheck", ["govulncheck", "./..."], ROOT/"backend")
            # pnpm uses 1 for reported advisories and for some operational
            # failures. Capture both, then require the existing CI validator
            # to reject malformed reports and unexcepted High/Critical issues.
            captured = run("pnpm-audit-capture", ["corepack", "pnpm", "audit", "--prod", "--json"], ROOT/"frontend", accepted_codes=(0, 1), separate_stderr=True)
            ok &= captured
            if captured:
                ok &= run("pnpm-audit-policy", ["python3", "tools/check_pnpm_audit_exceptions.py", "--audit", str(logdir/"pnpm-audit-capture.log"), "--exceptions", ".github/audit-exceptions.yml"])
    report = {"complete_run": args.only is None, "success": bool(ok), "results": results,
              "limitations": "Performance, two-replica recovery and visual/manual review require separate evidence; no deployment performed"}
    (logdir/"report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2)+"\n")
    print(f"Report: {logdir/'report.json'}", flush=True)
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
