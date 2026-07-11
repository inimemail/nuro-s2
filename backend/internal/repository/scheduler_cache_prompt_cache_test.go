package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilterSchedulerCredentialsKeepsPromptCacheAdvancedFlags(t *testing.T) {
	credentials := map[string]any{
		"prompt_cache_boost_enabled":                       true,
		"prompt_cache_boost_level":                         "aggressive",
		"prompt_cache_boost_upstream_hit_priority_enabled": true,
		"prompt_cache_smart_routing_enabled":               true,
		"prompt_cache_account_relay_enabled":               true,
		"prompt_cache_key_optimization_enabled":            true,
		"prompt_cache_long_context_enhancement_enabled":    true,
		"unrelated_secret":                                 "drop-me",
	}

	filtered := filterSchedulerCredentials(credentials)
	require.NotContains(t, filtered, "unrelated_secret")
	for key, value := range credentials {
		if key == "unrelated_secret" {
			continue
		}
		require.Equal(t, value, filtered[key], key)
	}
}

func TestFilterSchedulerCredentialsDoesNotExposeLegacyCacheStrategyWithoutAdvancedFlags(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"prompt_cache_boost_enabled":                       true,
		"prompt_cache_boost_level":                         "aggressive",
		"prompt_cache_boost_upstream_hit_priority_enabled": true,
	})

	require.Equal(t, true, filtered["prompt_cache_boost_enabled"])
	require.NotContains(t, filtered, "prompt_cache_boost_level")
	require.NotContains(t, filtered, "prompt_cache_boost_upstream_hit_priority_enabled")
}
