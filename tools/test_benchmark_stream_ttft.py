import unittest
from benchmark_stream_ttft import SSEParser, classify, measure, percentile, summary


class StreamTimingTest(unittest.TestCase):
    def test_split_utf8_and_crlf(self):
        source = 'data: {"type":"response.output_text.delta","delta":"中文"}\r\n\r\n'.encode()
        parser = SSEParser()
        frames = []
        for byte in source:
            frames.extend(parser.feed(bytes([byte])))
        self.assertEqual([classify(frame) for frame in frames], ["visible"])
        self.assertEqual(parser.feed(b"", eof=True), [])

    def test_only_real_content_is_visible(self):
        for payload in ['{"choices":[{"delta":{"role":"assistant"}}]}', '{"type":"response.created"}', '{"type":"ping"}']:
            self.assertEqual(classify(["data: " + payload]), "structure")
        self.assertEqual(classify([": ping"]), "progress")
        self.assertEqual(classify(['data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{"}}]}}]}']), "visible")

    def test_empty_error_and_duplicate_terminals(self):
        error = measure([b'data: {"type":"response.failed"}\n\n'])
        self.assertIsNone(error["first_visible_ms"])
        self.assertEqual(error["failure"], "upstream_terminal")
        self.assertIsNone(error["provider_ttft_ms"])
        duplicate = measure([b'data: {"type":"response.completed"}\n\n' * 2])
        self.assertEqual(duplicate["failure"], "terminal_count")
        self.assertTrue(measure([b'data: {"type":"response.completed"}'])["failure"])

    def test_hard_frame_limit_and_missing_percentiles(self):
        with self.assertRaises(ValueError):
            SSEParser(3).feed(b"data")
        self.assertIsNone(percentile([], .99))
        report = summary([dict(first_visible_ms=None, failure="empty")])
        self.assertIsNone(report["visible_p99_ms"])
        self.assertEqual(report["missing_visible"], 1)

    def test_request_start_is_included_in_timing(self):
        values = iter([2.0, 2.1])
        result = measure([b'data: {"type":"response.completed"}\n\n'], clock=lambda: next(values), started=1.0)
        self.assertEqual(result["first_byte_ms"], 1000)
        self.assertEqual(result["terminal_ms"], 1000)

    def test_transport_progress_is_not_the_first_arbitrary_event(self):
        chunks = [b'data: {"type":"response.created"}\n\n',
                  b'data: {"type":"response.transport_progress.delta","delta":"in_progress"}\n\n',
                  b'data: {"type":"response.output_text.delta","delta":"text"}\n\n',
                  b'data: {"type":"response.completed"}\n\n']
        times = iter([1, 2, 3, 4, 5])
        result = measure(chunks, clock=lambda: next(times), started=0)
        self.assertEqual(result["first_byte_ms"], 1000)
        self.assertEqual(result["first_progress_ms"], 2000)
        self.assertEqual(result["first_visible_ms"], 3000)
        self.assertIsNone(result["provider_ttft_ms"])
        no_progress = measure([chunks[0], chunks[2], chunks[3]])
        self.assertIsNone(no_progress["first_progress_ms"])
        self.assertEqual(classify([]), "structure")

    def test_summary_keeps_missing_timings_out_of_percentiles_and_counts_failures(self):
        rows = [dict(first_visible_ms=10, failure=None, terminals=1, first_byte_ms=2, headers_ms=1,
                     terminal_ms=20, total_ms=21),
                dict(first_visible_ms=None, failure="request_failed", terminals=0, total_ms=30),
                dict(first_visible_ms=5, failure="terminal_count", terminals=2, done_markers=2,
                     first_byte_ms=3, total_ms=40)]
        report = summary(rows)
        self.assertEqual(report["failures"], 2)
        self.assertEqual(report["missing_terminal"], 1)
        self.assertEqual(report["duplicate_terminal"], 1)
        self.assertEqual(report["failure_categories"], {"request_failed": 1, "terminal_count": 1})
        self.assertEqual(report["visible_p99_ms"], 10)
        self.assertEqual(report["first_byte"], {"samples": 2, "p50_ms": 2, "p95_ms": 3, "p99_ms": 3})
        self.assertEqual(report["first_reasoning"]["samples"], 0)
        self.assertIsNone(report["first_reasoning"]["p99_ms"])
        self.assertIsNone(report["provider_ttft_ms"])

    def test_run_deadline_terminates_slow_network_worker(self):
        import subprocess
        import sys
        import threading
        import time
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
        from pathlib import Path
        import tempfile
        class Slow(BaseHTTPRequestHandler):
            def do_POST(self):
                time.sleep(1)
            def log_message(self, *args):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Slow)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.NamedTemporaryFile() as body:
                body.write(b"{}"); body.flush()
                started = time.monotonic()
                run = subprocess.run([sys.executable, str(Path(__file__).with_name("benchmark_stream_ttft.py")),
                                      "--url", "http://127.0.0.1:" + str(server.server_port),
                                      "--body-file", body.name, "--duration", "0.25"],
                                     capture_output=True, text=True, timeout=5)
                self.assertEqual(run.returncode, 1)
                self.assertIn("run_duration_limit", run.stdout)
                self.assertLess(time.monotonic() - started, 3)
        finally:
            server.shutdown(); server.server_close()
