import unittest
from secret_scan import MAX_BYTES, scan_bytes


class SecretScannerTests(unittest.TestCase):
    def test_safe_generated_fixtures_detected_without_content_in_result(self):
        for content in [b"sk-" + b"Q8x4" * 16, b"ghp_" + b"x" * 36, b"-----BEGIN " + b"PRIVATE KEY-----", b"AKIA" + b"A" * 16]:
            findings = scan_bytes("file with spaces.txt", content)
            self.assertEqual(1, len(findings))
            self.assertNotIn(content.decode(), repr(findings))

    def test_placeholder_and_binary_not_reported(self):
        self.assertEqual([], scan_bytes("docs.md", b"sk-example-token"))
        self.assertEqual([], scan_bytes("binary", b"\0sk-" + b"a" * 50))

    def test_waiver_requires_matching_rule_and_reason(self):
        token = b"ghp_" + b"x" * 36
        self.assertEqual([], scan_bytes("fixture", token + b" # secret-scan: allow github-token -- deliberately invalid fixture"))
        self.assertEqual(1, len(scan_bytes("fixture", token + b" # secret-scan: allow private-key -- deliberately invalid fixture")))

    def test_large_text_fails_closed(self):
        self.assertEqual("text-file-too-large", scan_bytes("huge", b"x" * (MAX_BYTES + 1))[0].rule)


if __name__ == "__main__":
    unittest.main()
