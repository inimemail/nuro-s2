import unittest

from check_go_test_results import check


class GoTestResultTests(unittest.TestCase):
    def test_requires_actual_completed_tests(self):
        self.assertTrue(check([]))
        self.assertTrue(check([{"Action": "pass", "Package": "example"}]))
        self.assertEqual([], check([{"Action": "pass", "Test": "TestActual"}]))

    def test_required_test_cannot_be_missing_or_only_started(self):
        for events in [[], [{"Action": "run", "Test": "TestRequired"}],
                       [{"Action": "pass", "Test": "TestDifferent"}]]:
            self.assertTrue(check(events, ["TestRequired"]))

    def test_critical_skip_fails_even_if_another_test_passed(self):
        for name in ["TestScopedAdminPostgresTransactions", "TestUpgradeV0219MigrationContracts", "TestRedisLua",
                     "TestConcurrencyCacheSuite/TestGetAccountsLoadBatch",
                     "TestEdgeStreamRealRustTransportRecovery",
                     "TestEdgeWSRealRustHealthSettlement",
                     "TestEdgeWSRealRustFailedTurnReportsBeforeClientClose"]:
            events = [{"Action": "pass", "Test": "TestDifferent"},
                      {"Action": "skip", "Test": name}]
            self.assertTrue(check(events), name)

    def test_failures_cannot_be_hidden_by_parent_pass(self):
        events = [{"Action": "fail", "Test": "TestParent/child"},
                  {"Action": "pass", "Test": "TestParent"}]
        self.assertTrue(check(events))
        self.assertTrue(check([{"Action": "fail", "Package": "example"}]))

    def test_documented_optional_skip_does_not_mask_required_pass(self):
        events = [{"Action": "skip", "Test": "TestExternalTLSCapture"},
                  {"Action": "pass", "Test": "TestRequired"}]
        self.assertEqual([], check(events, ["TestRequired"]))


if __name__ == "__main__":
    unittest.main()
