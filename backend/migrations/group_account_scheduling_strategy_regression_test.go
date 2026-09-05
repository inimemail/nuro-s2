package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration223AddsGroupSchedulingStrategyAndInvalidatesAuthCache(t *testing.T) {
	content, err := FS.ReadFile("223_group_account_scheduling_strategy.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS account_scheduling_strategy")
	require.Contains(t, sql, "DEFAULT 'strict_priority'")
	require.Contains(t, sql, "NOT IN ('strict_priority', 'health_first')")
	require.Contains(t, sql, "trg_groups_account_scheduling_strategy_auth_cache_invalidation")
	require.Contains(t, sql, "AFTER UPDATE OF account_scheduling_strategy ON groups")
	require.Contains(t, sql, "enqueue_auth_cache_invalidation")
	require.NotContains(t, sql, "DROP COLUMN")
}

func TestMigration228AddsHealthCostBalancedSchedulingStrategy(t *testing.T) {
	content, err := FS.ReadFile("228_add_health_cost_balanced_scheduling_strategy.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "health_cost_balanced")
	require.Contains(t, sql, "COMMENT ON COLUMN groups.account_scheduling_strategy")
	require.NotContains(t, sql, "ALTER TABLE groups")
}
