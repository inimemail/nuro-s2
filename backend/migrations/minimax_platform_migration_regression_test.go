package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMiniMaxPlatformMigrationIsEmbeddedAndUpdatesAllProviderConstraints(t *testing.T) {
	sql, err := FS.ReadFile("229_add_minimax_platform.sql")
	require.NoError(t, err)
	body := strings.ToLower(string(sql))
	require.Contains(t, body, "channel_monitors_provider_check")
	require.Contains(t, body, "channel_monitor_request_templates_provider_check")
	require.Contains(t, body, "composite_model_routes_target_platform_check")
	require.Contains(t, body, "user_platform_quotas_platform_check")
	require.Contains(t, body, "'minimax'")
}
