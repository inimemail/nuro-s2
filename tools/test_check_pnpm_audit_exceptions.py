import unittest

from check_pnpm_audit_exceptions import validate_audit_report


class AuditReportTests(unittest.TestCase):
    def report(self, **counts):
        totals = dict.fromkeys(("info", "low", "moderate", "high", "critical"), 0)
        totals.update(counts)
        return {"advisories": {}, "metadata": {"vulnerabilities": totals}}

    def test_clean_report(self):
        validate_audit_report(self.report())

    def test_report_with_vulnerabilities(self):
        report = self.report(high=1)
        report["advisories"] = {"1": {"severity": "high", "module_name": "xlsx"}}
        validate_audit_report(report)

    def test_rejects_failed_or_incomplete_reports(self):
        for report in (None, [], {}, {"error": {"code": "ENOTFOUND"}},
                       {"advisories": {}}, self.report(high=1)):
            with self.subTest(report=report), self.assertRaises(ValueError):
                validate_audit_report(report)

    def test_rejects_invalid_entries_and_totals(self):
        report = self.report()
        report["advisories"] = {"1": {"severity": "unknown"}}
        with self.assertRaises(ValueError):
            validate_audit_report(report)
        for count in (True, -1, "0"):
            with self.subTest(count=count), self.assertRaises(ValueError):
                validate_audit_report(self.report(high=count))

    def test_rejects_hidden_high_details(self):
        report = self.report(high=1)
        report["advisories"] = {"1": {"severity": "low"}}
        with self.assertRaises(ValueError):
            validate_audit_report(report)

    def test_rejects_partial_high_details(self):
        report = self.report(high=2)
        report["advisories"] = {"1": {"severity": "high", "module_name": "xlsx"}}
        with self.assertRaises(ValueError):
            validate_audit_report(report)
        report["metadata"]["vulnerabilities"]["high"] = 0
        with self.assertRaises(ValueError):
            validate_audit_report(report)

    def test_accepts_vulnerabilities_schema(self):
        report = self.report(high=1)
        report["vulnerabilities"] = {
            "xlsx": {"severity": "high", "via": [{"url": "https://example.com/advisory"}]}
        }
        validate_audit_report(report)

    def test_rejects_conflicting_mixed_schemas(self):
        report = self.report(high=1)
        report["advisories"] = {
            "1": {"severity": "high", "module_name": "xlsx",
                  "github_advisory_id": "GHSA-example"}
        }
        report["vulnerabilities"] = {
            "xlsx": {"severity": "critical", "via": [
                {"github_advisory_id": "GHSA-example"}
            ]}
        }
        with self.assertRaisesRegex(ValueError, "mixes vulnerability schemas"):
            validate_audit_report(report)

    def test_accepts_empty_alternative_schema(self):
        report = self.report(high=1)
        report["advisories"] = {"1": {"severity": "high", "module_name": "xlsx"}}
        report["vulnerabilities"] = {}
        validate_audit_report(report)

    def test_rejects_conflicting_duplicate_advisory(self):
        report = self.report(high=1, critical=1)
        report["advisories"] = {
            "1": {"severity": "high", "module_name": "xlsx",
                  "github_advisory_id": "GHSA-example"},
            "2": {"severity": "critical", "module_name": "xlsx",
                  "github_advisory_id": "ghsa-example"}
        }
        with self.assertRaisesRegex(ValueError, "conflicting advisory severities"):
            validate_audit_report(report)

    def test_accepts_consistent_duplicate_advisory(self):
        report = self.report(high=2)
        entry = {"severity": "high", "module_name": "xlsx",
                 "github_advisory_id": "GHSA-example"}
        report["advisories"] = {"1": entry, "2": entry.copy()}
        validate_audit_report(report)


if __name__ == "__main__":
    unittest.main()
