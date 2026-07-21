package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration195ResetsLegacyOverridesAndRefreshesSchedulerMetadata(t *testing.T) {
	content, err := FS.ReadFile("195_reset_legacy_account_group_billing_guard_overrides.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "UPDATE account_groups")
	require.Contains(t, sql, "SET upstream_billing_guard_max_multiplier = NULL")
	require.Contains(t, sql, "g.platform = 'openai'")
	require.Contains(t, sql, "'full_rebuild'")
	require.Contains(t, sql, `"refresh_account_metadata":true`)
	require.Contains(t, sql, "reset_legacy_account_group_billing_guard_overrides_v5")
}
