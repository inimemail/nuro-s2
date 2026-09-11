package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupAdaptiveHealthSampleFreshnessMigration(t *testing.T) {
	content, err := FS.ReadFile("231_group_adaptive_health_sample_freshness.sql")
	require.NoError(t, err)
	sql := string(content)

	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS adaptive_health_sample_freshness_minutes")
	require.Contains(t, sql, "DEFAULT 15")
	require.Contains(t, sql, "adaptive_health_sample_freshness_minutes >= 1")
	require.Contains(t, sql, "adaptive_health_sample_freshness_minutes <= 120")
	require.Contains(t, sql, "adaptive_health_sample_freshness_minutes ON groups")
	require.Contains(t, sql, "enqueue_group_account_scheduling_strategy_auth_cache_invalidation")

	entries, err := FS.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.Name() != "231_group_adaptive_health_sample_freshness.sql" {
			require.False(t, strings.HasPrefix(entry.Name(), "231_"), "migration prefix 231 must be unique")
		}
	}
}
