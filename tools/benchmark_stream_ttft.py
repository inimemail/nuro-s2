#!/usr/bin/env python3
"""Bounded SSE timing check. Offline by default; --url explicitly enables traffic.

No response bodies, prompts, headers or credentials are included in results.
A client SSE trace cannot prove provider origin: provider TTFT stays unknown.
"""
from __future__ import annotations

import argparse
import codecs
from concurrent.futures import ThreadPoolExecutor
import json
import math
import os
import subprocess
import sys
from pathlib import Path
import time
import urllib.request

MAX_FRAME = 1 << 20
MAX_BYTES = 16 << 20


class SSEParser:
    def __init__(self, max_frame=MAX_FRAME):
        self.decoder = codecs.getincrementaldecoder("utf-8")("strict")
        self.line = ""
        self.lines = []
        self.size = 0
        self.cr = False
        self.max_frame = max_frame

    def feed(self, data: bytes, eof=False):
        frames = []
        for char in self.decoder.decode(data, final=eof):
            if char == "\n" and self.cr:
                self.cr = False
                continue
            self.cr = char == "\r"
            self.size += len(char.encode("utf-8"))
            if self.size > self.max_frame:
                raise ValueError("frame_limit")
            if char not in "\r\n":
                self.line += char
                continue
            if self.line:
                self.lines.append(self.line)
                self.line = ""
            else:
                frames.append(self.lines)
                self.lines = []
                self.size = 0
        if eof and (self.line or self.lines):
            raise ValueError("incomplete_frame")
        return frames


def classify(frame, chat_choices=None):
    data = []
    event = ""
    for line in frame:
        if line.startswith(":"):
            continue
        key, sep, value = line.partition(":")
        if value.startswith(" "):
            value = value[1:]
        if key == "data" and sep:
            data.append(value)
        elif key == "event":
            event = value
    if not data:
        return "progress" if any(line.startswith(":") for line in frame) else "structure"
    payload = "\n".join(data)
    if payload == "[DONE]":
        return "done_marker"
    item = json.loads(payload)
    if not isinstance(item, dict):
        return "structure"
    typ = item.get("type", event)
    if typ == "response.transport_progress.delta" and item.get("delta") == "in_progress":
        return "progress"
    if typ in ("response.completed", "response.failed", "response.incomplete"):
        return "failed_terminal" if typ != "response.completed" else "terminal"
    if typ == "error" or "error" in item:
        return "failed_terminal"
    if typ in ("response.output_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta"):
        return "visible" if item.get("delta") else "structure"
    if typ in ("response.reasoning_text.delta", "response.reasoning_summary_text.delta"):
        return "reasoning" if item.get("delta") else "structure"
    choices = item.get("choices") or []
    if chat_choices is not None:
        for choice in choices:
            index = choice.get("index", 0)
            if type(index) is not int or index < 0:
                raise ValueError("invalid_choice")
            if chat_choices.get(index) is True:
                raise ValueError("duplicate_choice_terminal")
            chat_choices[index] = bool(choice.get("finish_reason"))
    for choice in choices:
        delta = choice.get("delta") or {}
        if delta.get("content") or any((call.get("function") or {}).get("arguments") for call in delta.get("tool_calls") or []):
            return "visible"
    if any((choice.get("delta") or {}).get("reasoning_content") for choice in choices):
        return "reasoning"
    if any(choice.get("finish_reason") for choice in choices):
        return "chat_choice_terminal" if chat_choices is not None else "terminal"
    return "structure"


def measure(chunks, clock=time.monotonic, started=None):
    parser = SSEParser()
    start = clock() if started is None else started
    result = dict(first_byte_ms=None, first_progress_ms=None, first_visible_ms=None,
                  first_reasoning_ms=None, provider_ttft_ms=None, terminal_ms=None,
                  terminals=0, done_markers=0, failure=None)
    size = 0
    chat_choices = {}
    last_chat_terminal_ms = None
    try:
        for chunk in chunks:
            now = (clock() - start) * 1000
            size += len(chunk)
            if size > MAX_BYTES:
                raise ValueError("response_limit")
            if chunk and result["first_byte_ms"] is None:
                result["first_byte_ms"] = now
            for frame in parser.feed(chunk):
                kind = classify(frame, chat_choices)
                if result["done_markers"] and kind != "done_marker" and any(line.startswith("data:") for line in frame):
                    raise ValueError("data_after_done")
                if kind == "chat_choice_terminal" or (chat_choices and all(chat_choices.values()) and kind in ("visible", "reasoning")):
                    last_chat_terminal_ms = now
                if kind == "progress" and result["first_progress_ms"] is None:
                    result["first_progress_ms"] = now
                if kind in ("visible", "reasoning"):
                    key = "first_visible_ms" if kind == "visible" else "first_reasoning_ms"
                    if result[key] is None:
                        result[key] = now
                if kind == "done_marker":
                    if chat_choices and not all(chat_choices.values()):
                        raise ValueError("unfinished_chat_choice")
                    result["done_markers"] += 1
                elif kind in ("terminal", "failed_terminal"):
                    result["terminals"] += 1
                    result["terminal_ms"] = now
                    if kind == "failed_terminal":
                        result["failure"] = "upstream_terminal"
        parser.feed(b"", eof=True)
        if chat_choices and all(chat_choices.values()):
            result["terminals"] += 1
            result["terminal_ms"] = last_chat_terminal_ms
        if result["terminals"] != 1 or result["done_markers"] > 1:
            result["failure"] = "terminal_count"
        elif chat_choices and result["done_markers"] != 1:
            result["failure"] = "missing_done_marker"
    except (ValueError, UnicodeError, TypeError, AttributeError):
        result["failure"] = "invalid_or_oversized_stream"
    result["total_ms"] = (clock() - start) * 1000
    return result


def percentile(values, fraction):
    if not values:
        return None
    ordered = sorted(values)
    return ordered[max(0, math.ceil(len(ordered) * fraction) - 1)]


def summary(results):
    visible = [r["first_visible_ms"] for r in results if r["first_visible_ms"] is not None and not r["failure"]]
    report = dict(samples=len(results), failures=sum(bool(r["failure"]) for r in results),
                missing_visible=sum(r["first_visible_ms"] is None for r in results),
                missing_terminal=sum(r.get("terminals", 0) == 0 for r in results),
                duplicate_terminal=sum(r.get("terminals", 0) > 1 or r.get("done_markers", 0) > 1 for r in results),
                provider_ttft_ms=None, visible_p50_ms=percentile(visible, .5),
                visible_p95_ms=percentile(visible, .95), visible_p99_ms=percentile(visible, .99))
    for key in ("headers", "first_byte", "first_progress", "first_reasoning", "terminal", "total"):
        values = [r[key+"_ms"] for r in results if r.get(key+"_ms") is not None]
        report[key] = {"samples": len(values), "p50_ms": percentile(values, .5),
                       "p95_ms": percentile(values, .95), "p99_ms": percentile(values, .99)}
    categories = {}
    for result in results:
        if result["failure"]:
            categories[result["failure"]] = categories.get(result["failure"], 0) + 1
    report["failure_categories"] = categories
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", type=Path)
    parser.add_argument("--url")
    parser.add_argument("--body-file", type=Path)
    parser.add_argument("--concurrency", type=int, default=1)
    parser.add_argument("--requests", type=int, default=1)
    parser.add_argument("--timeout", type=float, default=30)
    parser.add_argument("--duration", type=float, default=60,
                        help="hard limit for the complete run, seconds (0,600]")
    parser.add_argument("--worker", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if not (1 <= args.concurrency <= 160 and 1 <= args.requests <= 1000 and 0 < args.timeout <= 120):
        parser.error("limits: concurrency 1..160, requests 1..1000, timeout (0,120]")
    if not math.isfinite(args.duration) or not 0 < args.duration <= 600:
        parser.error("duration must be in (0,600]")
    if not args.worker:
        # Isolate worker threads so DNS, a slow read, or executor shutdown cannot
        # hold the complete run open beyond its deadline. No response data leaves
        # the worker; the single bounded summary is the only output.
        try:
            child = subprocess.run([sys.executable, str(Path(__file__).resolve()),
                                    *sys.argv[1:], "--worker"],
                                   timeout=args.duration, capture_output=True, text=True)
        except subprocess.TimeoutExpired:
            print(json.dumps(dict(samples=0, failures=1, failure="run_duration_limit",
                                  incomplete=True, offline=not bool(args.url),
                                  concurrency=args.concurrency, provider_ttft_ms=None)))
            return 1
        if child.returncode not in (0, 1) or not child.stdout.strip():
            print(json.dumps(dict(samples=0, failures=1, failure="invalid_configuration_or_worker")))
            return 1
        print(child.stdout.strip())
        return child.returncode
    if args.url and (args.fixture or not args.body_file):
        parser.error("--url requires --body-file and excludes --fixture")
    if args.url and not args.url.startswith(("http://", "https://")):
        parser.error("unsupported endpoint scheme")
    if args.fixture and args.fixture.stat().st_size > MAX_BYTES:
        parser.error("fixture exceeds limit")
    if args.body_file and args.body_file.stat().st_size > MAX_FRAME:
        parser.error("request exceeds limit")
    fixture = args.fixture.read_bytes() if args.fixture else b'data: {"type":"response.output_text.delta","delta":"fixture"}\n\ndata: {"type":"response.completed"}\n\n'
    body = args.body_file.read_bytes() if args.body_file else None
    def run(_):
        if not args.url:
            return measure([fixture])
        started = time.monotonic()
        try:
            headers = {"Content-Type": "application/json", "Accept": "text/event-stream"}
            if os.environ.get("NURO_BENCH_API_KEY"):
                headers["Authorization"] = "Bearer " + os.environ["NURO_BENCH_API_KEY"]
            request = urllib.request.Request(args.url, data=body, headers=headers, method="POST")
            # Refuse redirects: credentials and POST traffic stay at the supplied endpoint.
            class NoRedirect(urllib.request.HTTPRedirectHandler):
                def redirect_request(self, *a, **kw):
                    return None
            with urllib.request.build_opener(NoRedirect).open(request, timeout=args.timeout) as response:
                def chunks():
                    while True:
                        if time.monotonic() - started > args.timeout:
                            raise TimeoutError("duration_limit")
                        data = response.read1(4096)
                        if not data:
                            return
                        yield data
                headers_ms = (time.monotonic() - started) * 1000
                result = measure(chunks(), started=started)
                result["headers_ms"] = headers_ms
                return result
        except Exception:
            return dict(first_visible_ms=None, failure="request_failed")
    with ThreadPoolExecutor(max_workers=args.concurrency) as executor:
        results = list(executor.map(run, range(args.requests)))
    report = summary(results)
    report.update(concurrency=args.concurrency, offline=not bool(args.url))
    print(json.dumps(report, allow_nan=False))
    return int(report["failures"] > 0)


if __name__ == "__main__":
    raise SystemExit(main())
