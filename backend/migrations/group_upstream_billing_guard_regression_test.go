package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration189MovesGuardPolicyToGroupsWithoutDroppingLegacyData(t *testing.T) {
	content, err := FS.ReadFile("189_group_upstream_billing_guard.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS upstream_billing_guard_max_multiplier DOUBLE PRECISION NULL")
	require.Contains(t, sql, "MIN(ag.upstream_billing_guard_max_multiplier)")
	require.Contains(t, sql, "g.platform = 'openai'")
	require.Contains(t, sql, "g.upstream_billing_guard_max_multiplier IS NOT NULL")
	require.Contains(t, sql, "upstream_billing_guard_enabled = TRUE")
	require.Contains(t, sql, `{"upstream_billing_probe_enabled":true}`)
	require.Contains(t, sql, "'full_rebuild'")
	require.Contains(t, sql, "group_upstream_billing_guard_v2")
	require.Contains(t, sql, `"refresh_account_metadata":true`)
	require.NotContains(t, sql, "DROP COLUMN")
	require.NotContains(t, sql, "DELETE FROM account_groups")
}
