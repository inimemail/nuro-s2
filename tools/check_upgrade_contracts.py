#!/usr/bin/env python3
"""Read-only upgrade fence. No application startup work or production access."""
from collections import defaultdict
import argparse
from hashlib import sha256
import json
from pathlib import Path
import re
import sys
import subprocess


def safe_file(root, name):
    relative = Path(name)
    if relative.is_absolute() or not relative.parts or ".." in relative.parts:
        return None
    path = root / relative
    if any((root / Path(*relative.parts[:i])).is_symlink() for i in range(1, len(relative.parts) + 1)):
        return None
    if not path.is_file() or not path.resolve().is_relative_to(root.resolve()):
        return None
    return path


def check_protected(root, manifest):
    failures = []
    for name, expected in manifest["files"].items():
        path = safe_file(root, name)
        if path is None or sha256(path.read_bytes()).hexdigest() != expected:
            failures.append(f"protected file changed: {name}")
    return failures


def check_historical_migrations(root, manifest):
    return check_protected(root, {"files": {
        "backend/migrations/" + name: digest
        for name, digest in manifest.get("migrations", {}).items()
    }})


def validate_manifest(manifest):
    if not isinstance(manifest, dict) or not re.fullmatch(r"[0-9a-f]{40}", str(manifest.get("baseline", ""))):
        raise ValueError("manifest requires an immutable 40-character baseline SHA")
    for field in ("files", "migrations"):
        values = manifest.get(field, {})
        if not isinstance(values, dict) or (field == "files" and not values):
            raise ValueError(f"invalid manifest {field}")
        if any(not isinstance(k, str) or not isinstance(v, str) or not re.fullmatch(r"[0-9a-f]{64}", v) for k, v in values.items()):
            raise ValueError(f"invalid SHA-256 in {field}")
        if field == "migrations" and any(Path(k).name != k or not k.endswith(".sql") for k in values):
            raise ValueError("invalid historical migration filename")
    prefixes = manifest.get("existing_migration_prefixes")
    if not isinstance(prefixes, dict) or any(not isinstance(v, list) or any(not isinstance(n, str) for n in v) for v in prefixes.values()):
        raise ValueError("invalid existing migration prefixes")


def check_baseline(root, manifest, baseline):
    """A changed manifest must not silently bless edits to protected sources."""
    if baseline != manifest["baseline"]:
        return ["manifest baseline differs from approved baseline"]
    failures = []
    paths = dict(manifest["files"])
    paths.update({"backend/migrations/" + n: h for n, h in manifest.get("migrations", {}).items()})
    for name, expected in paths.items():
        result = subprocess.run(["git", "show", f"{baseline}:{name}"], cwd=root, capture_output=True)
        if result.returncode or sha256(result.stdout).hexdigest() != expected:
            failures.append(f"manifest does not match approved baseline: {name}")
    return failures


def check_go_images(root):
    go_match = re.search(r"^go (\S+)$", (root / "backend/go.mod").read_text(), re.M)
    if go_match is None:
        return ["missing Go module directive"]
    go = go_match.group(1)
    failures = []
    for name, pattern in {
        "Dockerfile": r"^ARG GOLANG_IMAGE=golang:([^\s]+)-alpine$",
        "backend/Dockerfile": r"^FROM golang:([^\s]+)-alpine$",
    }.items():
        match = re.search(pattern, (root / name).read_text(), re.M)
        if match is None or match.group(1) != go:
            failures.append(f"Go/Docker mismatch: {name}; go.mod uses {go}")
    return failures


def check_migrations(directory, baseline_names=()):
    # This project identifies migrations by full filename. Preserve historical
    # shared numeric prefixes, but reject newly introduced prefix collisions.
    baseline = defaultdict(set)
    for name in baseline_names:
        match = re.match(r"^(\d+)_", name)
        if match:
            baseline[int(match.group(1))].add(name)
    versions = defaultdict(list)
    for path in directory.glob("*.sql"):
        match = re.match(r"^(\d+)_", path.name)
        if match:
            versions[int(match.group(1))].append(path.name)
    return [f"duplicate migration {version}: {', '.join(names)}" for version, names in sorted(versions.items()) if len(names) > 1 and set(names) != baseline[version]]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", default="docs/upgrades/v0219-protected.json")
    parser.add_argument("--baseline", default="861d2874a5e87706465ab53170736b97c1e8ccdb")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    try:
        path = safe_file(root, args.manifest)
        if path is None:
            raise ValueError("manifest missing or unsafe")
        manifest = json.loads(path.read_text())
        validate_manifest(manifest)
    except (ValueError, OSError) as error:
        print(f"Invalid upgrade manifest: {error}", file=sys.stderr)
        return 1
    baseline_names = [name for group in manifest["existing_migration_prefixes"].values() for name in group]
    failures = (check_protected(root, manifest) + check_historical_migrations(root, manifest)
                + check_migrations(root / "backend/migrations", baseline_names)
                + check_baseline(root, manifest, args.baseline) + check_go_images(root))
    if failures:
        print("\n".join(failures), file=sys.stderr)
        return 1
    print(f"PASS: {len(manifest['files'])} protected files and {len(manifest.get('migrations', {}))} historical migrations unchanged; approved baseline; no new migration collisions; Go/Docker aligned")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
