#!/usr/bin/env python3
"""Reject missing or silently skipped critical Go integration tests."""
import argparse
import json
from pathlib import Path
import re
import sys


def check(events, required=()):
    failures = []
    passed = set()
    critical = re.compile(r"Integration|Postgres|Redis|Lua|Idempotency|Account|Group|Quota|MigrationContracts|Edge(?:Stream|WS)RealRust", re.I)
    for event in events:
        name = event.get("Test", "")
        action = event.get("Action")
        if action == "pass" and name:
            passed.add(name)
        if action == "fail":
            failures.append("failed: " + (name or event.get("Package", "unknown")))
        if action == "skip" and name and (critical.search(name) or name in required):
            failures.append("critical test skipped: " + name)
    failures.extend("required test did not pass: " + name for name in required if name not in passed)
    if not passed:
        failures.append("no completed tests")
    return failures


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("log", type=Path)
    parser.add_argument("--require-test", action="append", default=[])
    args = parser.parse_args()
    try:
        events = [json.loads(line) for line in args.log.read_text().splitlines() if line.strip()]
        if any(not isinstance(event, dict) for event in events):
            raise ValueError("invalid event")
    except (OSError, ValueError):
        print("Invalid or missing Go JSON test log", file=sys.stderr)
        return 1
    errors = check(events, args.require_test)
    print("\n".join(errors) if errors else "PASS: critical integration tests executed")
    return int(bool(errors))


if __name__ == "__main__":
    raise SystemExit(main())
