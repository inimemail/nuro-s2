package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFilterSchedulerCredentialsKeepsPromptCacheAdvancedFlags(t *testing.T) {
	credentials := map[string]any{
		"prompt_cache_boost_enabled":                        true,
		"prompt_cache_boost_level":                          "aggressive",
		"prompt_cache_boost_upstream_hit_priority_enabled":  true,
		"prompt_cache_smart_routing_enabled":                true,
		"prompt_cache_account_relay_enabled":                true,
		"prompt_cache_key_optimization_enabled":             true,
		"prompt_cache_long_context_enhancement_enabled":     true,
		"openai_prompt_cache_creation_optimization_enabled": true,
		"openai_prompt_cache_creation_optimization_mode":    service.OpenAIPromptCacheCreationOptimizationModeSuppress,
		"unrelated_secret":                                  "drop-me",
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

func TestBuildSchedulerMetadataAccountKeepsPromptCacheCreationOptimization(t *testing.T) {
	account := service.Account{
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"openai_prompt_cache_creation_optimization_enabled": true,
			"openai_prompt_cache_creation_optimization_mode":    service.OpenAIPromptCacheCreationOptimizationModeSuppress,
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.True(t, got.IsOpenAIPromptCacheCreationOptimizationEnabled())
	require.True(t, got.IsOpenAIPromptCacheCreationSuppressEnabled())
}

func TestFilterSchedulerCredentialsKeepsCacheAffinityModeWithoutChildFeatures(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"prompt_cache_boost_enabled":                       true,
		"prompt_cache_boost_level":                         "aggressive",
		"prompt_cache_boost_upstream_hit_priority_enabled": true,
	})

	require.Equal(t, true, filtered["prompt_cache_boost_enabled"])
	require.Equal(t, "aggressive", filtered["prompt_cache_boost_level"])
	require.Equal(t, true, filtered["prompt_cache_boost_upstream_hit_priority_enabled"])
}

func TestBuildSchedulerMetadataAccountPreservesSelectionSemantics(t *testing.T) {
	t.Run("openai privacy passthrough and custom endpoint", func(t *testing.T) {
		account := service.Account{
			ID:          501,
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Credentials: map[string]any{
				"base_url": "https://private-upstream.example/v1",
			},
			Extra: map[string]any{
				"privacy_mode":       service.PrivacyModeTrainingOff,
				"openai_passthrough": true,
			},
		}

		got := buildSchedulerMetadataAccount(account)

		require.Equal(t, account.IsPrivacySet(), got.IsPrivacySet())
		require.Equal(t, account.IsOpenAIPassthroughEnabled(), got.IsOpenAIPassthroughEnabled())
		require.Equal(t, account.IsModelSupported("vendor-private-model"), got.IsModelSupported("vendor-private-model"))
		require.Equal(t, account.GetCredential("base_url"), got.GetCredential("base_url"))
	})

	t.Run("openai alpha search custom base url remains eligible", func(t *testing.T) {
		account := service.Account{
			ID:          502,
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Schedulable: true,
			Credentials: map[string]any{
				"base_url": "https://private-upstream.example/v1",
			},
		}

		got := buildSchedulerMetadataAccount(account)

		require.True(t, service.IsOpenAIAlphaSearchAccountEligible(&account))
		require.True(t, service.IsOpenAIAlphaSearchAccountEligible(&got))
	})

	t.Run("api key quota exclusion", func(t *testing.T) {
		account := service.Account{
			ID:          503,
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Schedulable: true,
			Extra: map[string]any{
				"quota_limit": 10.0,
				"quota_used":  10.0,
			},
		}

		got := buildSchedulerMetadataAccount(account)

		require.False(t, account.IsSchedulable())
		require.Equal(t, account.IsSchedulable(), got.IsSchedulable())
	})

	t.Run("anthropic rpm controls", func(t *testing.T) {
		account := service.Account{
			ID:          504,
			Platform:    service.PlatformAnthropic,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Extra: map[string]any{
				"base_rpm":          120,
				"rpm_strategy":      "sticky_exempt",
				"rpm_sticky_buffer": 17,
			},
		}

		got := buildSchedulerMetadataAccount(account)

		require.Equal(t, account.GetBaseRPM(), got.GetBaseRPM())
		require.Equal(t, account.GetRPMStrategy(), got.GetRPMStrategy())
		require.Equal(t, account.GetRPMStickyBuffer(), got.GetRPMStickyBuffer())
	})

	t.Run("antigravity overages flag", func(t *testing.T) {
		account := service.Account{
			ID:          505,
			Platform:    service.PlatformAntigravity,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Extra: map[string]any{
				"allow_overages": true,
			},
		}

		got := buildSchedulerMetadataAccount(account)

		require.Equal(t, account.IsOveragesEnabled(), got.IsOveragesEnabled())
		require.Equal(t, account.IsSchedulableForModelWithContext(context.Background(), "gemini-3.1-pro"), got.IsSchedulableForModelWithContext(context.Background(), "gemini-3.1-pro"))
	})
}
