#!/usr/bin/env python3
"""Read-only upgrade fence. No application startup work or production access."""
from collections import defaultdict
from hashlib import sha256
import json
from pathlib import Path
import re
import sys


def check_protected(root, manifest):
    failures = []
    for name, expected in manifest["files"].items():
        relative = Path(name)
        if relative.is_absolute() or ".." in relative.parts:
            failures.append(f"invalid protected path: {name}")
            continue
        path = root / relative
        if not path.is_file() or sha256(path.read_bytes()).hexdigest() != expected:
            failures.append(f"protected file changed: {name}")
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
    root = Path(__file__).resolve().parents[1]
    manifest = json.loads((root / "docs/upgrades/v0218-protected.json").read_text())
    baseline_names = [name for group in manifest["existing_migration_prefixes"].values() for name in group]
    failures = check_protected(root, manifest) + check_migrations(root / "backend/migrations", baseline_names)
    go = re.search(r"^go (\S+)$", (root / "backend/go.mod").read_text(), re.M).group(1)
    images = {
        "Dockerfile": re.search(r"^ARG GOLANG_IMAGE=golang:([^\s]+)-alpine$", (root / "Dockerfile").read_text(), re.M).group(1),
        "backend/Dockerfile": re.search(r"^FROM golang:([^\s]+)-alpine$", (root / "backend" / "Dockerfile").read_text(), re.M).group(1),
    }
    for path, image in images.items():
        if go != image:
            failures.append(f"Go/Docker mismatch: {path} uses {image}; go.mod uses {go}")
    if failures:
        print("\n".join(failures), file=sys.stderr)
        return 1
    print(f"PASS: {len(manifest['files'])} protected files unchanged; no new migration prefix collisions; Go/Docker {go}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
