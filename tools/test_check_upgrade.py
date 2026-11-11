import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from check_upgrade import ROOT, REQUIRED_TESTS, go_test_counts, rust_version_supported, preflight, main


class UpgradeRunnerTests(unittest.TestCase):
    def test_rust_uses_ci_compatible_stable_version(self):
        self.assertTrue(rust_version_supported('rustc 1.90.0 (fixture)', '1.90.0'))
        self.assertTrue(rust_version_supported('rustc 1.96.1 (fixture)', '1.90.0'))
        for actual in ('rustc 1.89.9 (fixture)', 'rustc 1.90.0-nightly (fixture)', '', 'unknown'):
            self.assertFalse(rust_version_supported(actual, '1.90.0'))

    def test_preflight_rejects_each_mismatched_toolchain(self):
        versions = ['go1.27.4', 'v24.13.0', '9.15.9', 'rustc 1.96.1 (fixture)']
        with patch('check_upgrade.subprocess.check_output', side_effect=versions):
            self.assertTrue(preflight())
        for index in range(3):
            mismatched = versions.copy()
            mismatched[index] = 'unexpected-version'
            with self.subTest(tool=index), patch('check_upgrade.subprocess.check_output', side_effect=mismatched) as command:
                self.assertFalse(preflight())
                self.assertEqual(command.call_count, index + 1)

        with patch('check_upgrade.subprocess.check_output', return_value='go1.27.3') as command:
            self.assertFalse(preflight())
            self.assertEqual(command.call_count, 1)

    def test_counts_only_completed_tests(self):
        lines = ['not-json', '[]', json.dumps({'Action': 'pass', 'Package': 'fixture'})]
        lines += [json.dumps({'Test': 'TestFixture', 'Action': action}) for action in ('run', 'pass', 'skip', 'fail')]
        self.assertEqual(go_test_counts('\n'.join(lines)), {'pass': 1, 'fail': 1, 'skip': 1})

    def test_required_tests_exist_in_repository(self):
        sources = '\n'.join(p.read_text() for p in (ROOT/'backend').rglob('*_test.go'))
        for name in REQUIRED_TESTS:
            self.assertIn('func '+name.split('/')[0]+'(', sources, name)

    def test_audit_stderr_is_retained_without_corrupting_json(self):
        real_run = subprocess.run
        payload = {'advisories': {}, 'metadata': {'vulnerabilities': dict.fromkeys(('info', 'low', 'moderate', 'high', 'critical'), 0)}}
        for valid in (True, False):
            with self.subTest(valid=valid), tempfile.TemporaryDirectory() as temp:
                def fixture_run(command, **kwargs):
                    if command[:3] == ['corepack', 'pnpm', 'audit']:
                        kwargs['stdout'].write(json.dumps(payload) if valid else 'invalid-audit')
                        kwargs['stdout'].flush()
                        kwargs['stderr'].write('fixture runtime warning\n')
                        return subprocess.CompletedProcess(command, 1)
                    if 'tools/check_pnpm_audit_exceptions.py' in command:
                        return real_run(command, **kwargs)
                    return subprocess.CompletedProcess(command, 0)
                with patch('check_upgrade.preflight', return_value=True), patch('check_upgrade.subprocess.run', side_effect=fixture_run), patch.object(sys, 'argv', ['check_upgrade.py', '--only', 'security', '--log-dir', temp]):
                    self.assertEqual(main(), 0 if valid else 1)
                report = json.loads((Path(temp)/'report.json').read_text())
                self.assertEqual(report['success'], valid)
                audit = next(row for row in report['results'] if row['name'] == 'pnpm-audit-capture')
                self.assertEqual(audit['exit_code'], 1)
                self.assertIn('fixture runtime warning', Path(audit['stderr_log']).read_text())
                self.assertNotIn('fixture runtime warning', Path(audit['log']).read_text())

    def test_missing_toolchain_produces_failure_report_without_running_gates(self):
        with tempfile.TemporaryDirectory() as temp:
            result = subprocess.run(
                ['/usr/bin/env', 'python3', str(ROOT/'tools/check_upgrade.py'), '--only', 'contracts', '--log-dir', temp],
                env={'PATH': '/usr/bin:/bin'}, capture_output=True, text=True,
            )
            self.assertNotEqual(result.returncode, 0)
            report = json.loads((Path(temp)/'report.json').read_text())
            self.assertFalse(report['success'])
            self.assertFalse(report['complete_run'])
            self.assertEqual(report['results'], [{'name': 'toolchain-preflight', 'exit_code': 1}])


if __name__ == '__main__':
    unittest.main()
