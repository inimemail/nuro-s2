package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration222AddsGuardMinimumsAndPreservesMaximumOnlyPolicies(t *testing.T) {
	content, err := FS.ReadFile("222_upstream_billing_guard_min_multiplier.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "ALTER TABLE groups")
	require.Contains(t, sql, "ALTER TABLE account_groups")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS upstream_billing_guard_min_multiplier DOUBLE PRECISION NULL")
	require.Contains(t, sql, "upstream_billing_guard_min_multiplier >= 0")
	require.Contains(t, sql, "upstream_billing_guard_min_multiplier < 'Infinity'::double precision")
	require.Contains(t, sql, "upstream_billing_guard_min_multiplier < upstream_billing_guard_max_multiplier")
	require.Contains(t, sql, "trg_groups_billing_guard_min_auth_cache_invalidation")
	require.Contains(t, sql, "enqueue_auth_cache_invalidation")
	require.Contains(t, sql, "'full_rebuild'")
	require.Contains(t, sql, "upstream_billing_guard_min_multiplier_v1")
	require.NotContains(t, sql, "DROP COLUMN")
	require.NotContains(t, sql, "UPDATE groups SET upstream_billing_guard_max_multiplier")
}
