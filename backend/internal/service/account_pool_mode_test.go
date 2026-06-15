//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGetPoolModeRetryCount(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected int
	}{
		{
			name: "default_when_not_pool_mode",
			account: &Account{
				Type:        AccountTypeAPIKey,
				Platform:    PlatformOpenAI,
				Credentials: map[string]any{},
			},
			expected: defaultPoolModeRetryCount,
		},
		{
			name: "default_when_missing_retry_count",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode": true,
				},
			},
			expected: defaultPoolModeRetryCount,
		},
		{
			name: "supports_float64_from_json_credentials",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": float64(5),
				},
			},
			expected: 5,
		},
		{
			name: "supports_json_number",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": json.Number("4"),
				},
			},
			expected: 4,
		},
		{
			name: "supports_string_value",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": "2",
				},
			},
			expected: 2,
		},
		{
			name: "negative_value_is_clamped_to_zero",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": -1,
				},
			},
			expected: 0,
		},
		{
			name: "oversized_value_is_clamped_to_max",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": 99,
				},
			},
			expected: maxPoolModeRetryCount,
		},
		{
			name: "invalid_value_falls_back_to_default",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": "oops",
				},
			},
			expected: defaultPoolModeRetryCount,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, tt.account.GetPoolModeRetryCount())
		})
	}
}

func TestGetPoolModeSameAccountRetryDelay(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected time.Duration
	}{
		{
			name: "default_when_not_enabled",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode": true,
				},
			},
			expected: defaultPoolModeSameAccountRetryDelay,
		},
		{
			name: "uses_configured_delay",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":                                true,
					"upstream_concurrency_race_enabled":        true,
					"upstream_concurrency_race_retry_delay_ms": 50,
				},
			},
			expected: 50 * time.Millisecond,
		},
		{
			name: "clamps_below_minimum",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":                                true,
					"upstream_concurrency_race_enabled":        true,
					"upstream_concurrency_race_retry_delay_ms": 1,
				},
			},
			expected: minPoolModeSameAccountRetryDelay,
		},
		{
			name: "clamps_above_maximum",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":                                true,
					"upstream_concurrency_race_enabled":        true,
					"upstream_concurrency_race_retry_delay_ms": 999999,
				},
			},
			expected: maxPoolModeSameAccountRetryDelay,
		},
		{
			name: "invalid_delay_falls_back_to_default",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":                                true,
					"upstream_concurrency_race_enabled":        true,
					"upstream_concurrency_race_retry_delay_ms": "oops",
				},
			},
			expected: defaultPoolModeSameAccountRetryDelay,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, tt.account.GetPoolModeSameAccountRetryDelay())
		})
	}
}

func TestMatchesOpenAIImagePoolRequest(t *testing.T) {
	textPool := &Account{
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
	imagePool := &Account{
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"pool_mode":           true,
			"image_pool_mode":     true,
			"model_mapping":       map[string]any{"alias-image": "gpt-image-2"},
			"openai_capabilities": []any{"chat_completions"},
		},
	}
	plainAccount := &Account{
		Type:        AccountTypeAPIKey,
		Platform:    PlatformOpenAI,
		Credentials: map[string]any{},
	}

	require.True(t, plainAccount.MatchesOpenAIImagePoolRequest(context.Background(), "gpt-5.4", ""))
	require.True(t, textPool.MatchesOpenAIImagePoolRequest(context.Background(), "gpt-5.4", ""))
	require.False(t, textPool.MatchesOpenAIImagePoolRequest(context.Background(), "gpt-image-2", OpenAIImagesCapabilityBasic))
	require.False(t, imagePool.MatchesOpenAIImagePoolRequest(context.Background(), "gpt-5.4", ""))
	require.True(t, imagePool.MatchesOpenAIImagePoolRequest(context.Background(), "gpt-image-2", OpenAIImagesCapabilityNative))
	require.True(t, imagePool.MatchesOpenAIImagePoolRequest(WithOpenAIImageGenerationIntent(context.Background()), "gpt-5.4", ""))
	require.True(t, imagePool.MatchesOpenAIImagePoolRequest(context.Background(), "alias-image", ""))
}

func TestIsOpenAIPromptCacheBoostEnabled(t *testing.T) {
	textPool := &Account{
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"pool_mode":                  true,
			"prompt_cache_boost_enabled": true,
		},
	}
	imagePool := &Account{
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"pool_mode":                  true,
			"image_pool_mode":            true,
			"prompt_cache_boost_enabled": true,
		},
	}
	plain := &Account{
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"prompt_cache_boost_enabled": true,
		},
	}
	oauth := &Account{
		Type:     AccountTypeOAuth,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"pool_mode":                  true,
			"prompt_cache_boost_enabled": true,
		},
	}

	require.True(t, textPool.IsOpenAIPromptCacheBoostEnabled())
	require.False(t, imagePool.IsOpenAIPromptCacheBoostEnabled())
	require.False(t, plain.IsOpenAIPromptCacheBoostEnabled())
	require.False(t, oauth.IsOpenAIPromptCacheBoostEnabled())
}

func TestIsOpenAIUpstreamStrongIsolationEnabled(t *testing.T) {
	textPool := &Account{
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"pool_mode":                         true,
			"upstream_strong_isolation_enabled": true,
		},
	}
	imagePool := &Account{
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"pool_mode":                         true,
			"image_pool_mode":                   true,
			"upstream_strong_isolation_enabled": true,
		},
	}
	plain := &Account{
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"upstream_strong_isolation_enabled": true,
		},
	}
	oauth := &Account{
		Type:     AccountTypeOAuth,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"pool_mode":                         true,
			"upstream_strong_isolation_enabled": true,
		},
	}

	require.True(t, textPool.IsOpenAIUpstreamStrongIsolationEnabled())
	require.False(t, imagePool.IsOpenAIUpstreamStrongIsolationEnabled())
	require.False(t, plain.IsOpenAIUpstreamStrongIsolationEnabled())
	require.False(t, oauth.IsOpenAIUpstreamStrongIsolationEnabled())
}
