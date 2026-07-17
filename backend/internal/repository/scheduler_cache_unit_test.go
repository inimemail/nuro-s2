//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestBuildSchedulerMetadataAccount_KeepsOpenAIWSFlags(t *testing.T) {
	account := service.Account{
		ID:       42,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_enabled": true,
			"openai_oauth_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
			"openai_ws_force_http":                         true,
			"openai_responses_mode":                        "force_chat_completions",
			"openai_responses_supported":                   false,
			"mixed_scheduling":                             true,
			"unused_large_field":                           "drop-me",
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.Equal(t, true, got.Extra["openai_oauth_responses_websockets_v2_enabled"])
	require.Equal(t, service.OpenAIWSIngressModePassthrough, got.Extra["openai_oauth_responses_websockets_v2_mode"])
	require.Equal(t, true, got.Extra["openai_ws_force_http"])
	require.Equal(t, "force_chat_completions", got.Extra["openai_responses_mode"])
	require.Equal(t, false, got.Extra["openai_responses_supported"])
	require.Equal(t, true, got.Extra["mixed_scheduling"])
	require.Nil(t, got.Extra["unused_large_field"])
}

func TestBuildSchedulerMetadataAccount_KeepsAnthropicAPIKeyBehaviorFlags(t *testing.T) {
	account := service.Account{
		ID:       43,
		Platform: service.PlatformAnthropic,
		Type:     service.AccountTypeAPIKey,
		Extra: map[string]any{
			"anthropic_kiro":        false,
			"anthropic_passthrough": true,
			"web_search_emulation":  service.WebSearchModeDisabled,
			"unused_large_field":    "drop-me",
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.Contains(t, got.Extra, "anthropic_kiro")
	require.Equal(t, false, got.Extra["anthropic_kiro"])
	require.Equal(t, true, got.Extra["anthropic_passthrough"])
	require.Equal(t, service.WebSearchModeDisabled, got.Extra["web_search_emulation"])
	require.Nil(t, got.Extra["unused_large_field"])
	require.False(t, got.IsAnthropicKiroEnabled())
	require.True(t, got.IsAnthropicAPIKeyPassthroughEnabled())
	require.Equal(t, service.WebSearchModeDisabled, got.GetWebSearchEmulationMode())
}

func TestBuildSchedulerMetadataAccount_KeepsSlimGroupMembership(t *testing.T) {
	account := service.Account{
		ID:       42,
		Platform: service.PlatformAnthropic,
		GroupIDs: []int64{7, 9, 7, 0},
		AccountGroups: []service.AccountGroup{
			{
				AccountID: 42,
				GroupID:   7,
				Priority:  2,
				Account:   &service.Account{ID: 42, Name: "drop-from-metadata"},
				Group:     &service.Group{ID: 7, Name: "drop-from-metadata"},
			},
			{
				AccountID: 42,
				GroupID:   11,
				Priority:  3,
				Group:     &service.Group{ID: 11, Name: "drop-from-metadata"},
			},
			{
				AccountID: 42,
				GroupID:   0,
				Priority:  4,
			},
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.Equal(t, []int64{7, 9, 11}, got.GroupIDs)
	require.Len(t, got.AccountGroups, 2)
	require.Equal(t, int64(42), got.AccountGroups[0].AccountID)
	require.Equal(t, int64(7), got.AccountGroups[0].GroupID)
	require.Equal(t, 2, got.AccountGroups[0].Priority)
	require.Nil(t, got.AccountGroups[0].Account)
	require.Nil(t, got.AccountGroups[0].Group)
	require.Equal(t, int64(11), got.AccountGroups[1].GroupID)
	require.Nil(t, got.Groups)
}

func TestBuildSchedulerMetadataAccount_KeepsQuotaAutoPauseFields(t *testing.T) {
	account := service.Account{
		ID: 88,
		Extra: map[string]any{
			"codex_5h_used_percent":        12.34,
			"codex_7d_used_percent":        56.78,
			"codex_5h_reset_at":            "2026-05-29T10:00:00Z",
			"codex_7d_reset_at":            "2026-06-01T10:00:00Z",
			"codex_5h_reset_after_seconds": 300,
			"codex_7d_reset_after_seconds": 600,
			"codex_usage_updated_at":       "2026-05-29T09:00:00Z",
			"auto_pause_5h_threshold":      0.95,
			"auto_pause_7d_threshold":      0.96,
			"auto_pause_5h_disabled":       true,
			"auto_pause_7d_disabled":       false,
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.Equal(t, 12.34, got.Extra["codex_5h_used_percent"])
	require.Equal(t, 56.78, got.Extra["codex_7d_used_percent"])
	require.Equal(t, "2026-05-29T10:00:00Z", got.Extra["codex_5h_reset_at"])
	require.Equal(t, "2026-06-01T10:00:00Z", got.Extra["codex_7d_reset_at"])
	require.Equal(t, 300, got.Extra["codex_5h_reset_after_seconds"])
	require.Equal(t, 600, got.Extra["codex_7d_reset_after_seconds"])
	require.Equal(t, "2026-05-29T09:00:00Z", got.Extra["codex_usage_updated_at"])
	require.Equal(t, 0.95, got.Extra["auto_pause_5h_threshold"])
	require.Equal(t, 0.96, got.Extra["auto_pause_7d_threshold"])
	require.Equal(t, true, got.Extra["auto_pause_5h_disabled"])
	require.Equal(t, false, got.Extra["auto_pause_7d_disabled"])
}

func TestBuildSchedulerMetadataAccount_KeepsSparkShadowRoutingIdentity(t *testing.T) {
	parentID := int64(100)
	account := service.Account{
		ID:              200,
		Platform:        service.PlatformOpenAI,
		Type:            service.AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  service.QuotaDimensionSpark,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-5.3-codex-spark": "gpt-5.3-codex-spark",
			},
			"compact_model_mapping": map[string]any{
				"gpt-5.4": "gpt-5.4-openai-compact",
			},
			"access_token": "drop-me",
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.NotNil(t, got.ParentAccountID)
	require.Equal(t, parentID, *got.ParentAccountID)
	require.Equal(t, service.QuotaDimensionSpark, got.QuotaDimension)
	require.Equal(t, map[string]any{"gpt-5.3-codex-spark": "gpt-5.3-codex-spark"}, got.Credentials["model_mapping"])
	require.Equal(t, map[string]any{"gpt-5.4": "gpt-5.4-openai-compact"}, got.Credentials["compact_model_mapping"])
	require.Nil(t, got.Credentials["access_token"])
}

func TestBuildSchedulerMetadataAccount_KeepsConcurrencyRaceRetryControls(t *testing.T) {
	account := service.Account{
		ID:       201,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                                true,
			"pool_mode_retry_count":                    7,
			"upstream_concurrency_race_enabled":        true,
			"upstream_concurrency_race_retry_delay_ms": 35,
			"upstream_concurrency_race_max_elapsed_ms": 2500,
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.True(t, got.IsOpenAIUpstreamConcurrencyRaceEnabled())
	require.Equal(t, 7, got.GetPoolModeRetryCount())
	require.Equal(t, 35, int(got.GetPoolModeSameAccountRetryDelay().Milliseconds()))
	require.Equal(t, 2500, int(got.GetPoolModeSameAccountRetryMaxElapsed().Milliseconds()))
}

func TestBuildSchedulerMetadataAccount_KeepsOpenAILongContextBillingPolicy(t *testing.T) {
	account := service.Account{
		ID:       204,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra: map[string]any{
			"openai_long_context_billing_enabled": true,
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.True(t, got.IsOpenAILongContextBillingEnabled())
}

func TestBuildSchedulerMetadataAccount_KeepsUpstreamBillingGuardDecision(t *testing.T) {
	observed := 2.5
	evaluatedAt := time.Now().UTC()
	account := service.Account{
		ID:                                     206,
		UpstreamBillingGuardEnabled:            true,
		UpstreamBillingGuardMaxMultiplier:      2,
		UpstreamBillingGuardBlocked:            true,
		UpstreamBillingGuardObservedMultiplier: &observed,
		UpstreamBillingGuardEvaluatedAt:        &evaluatedAt,
	}

	got := buildSchedulerMetadataAccount(account)

	require.True(t, got.UpstreamBillingGuardEnabled)
	require.Equal(t, 2.0, got.UpstreamBillingGuardMaxMultiplier)
	require.True(t, got.UpstreamBillingGuardBlocked)
	require.NotNil(t, got.UpstreamBillingGuardObservedMultiplier)
	require.Equal(t, observed, *got.UpstreamBillingGuardObservedMultiplier)
	require.Equal(t, evaluatedAt, *got.UpstreamBillingGuardEvaluatedAt)
}

func TestBuildSchedulerMetadataAccount_KeepsOpenAICompactCapability(t *testing.T) {
	account := service.Account{
		ID:       205,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra: map[string]any{
			"openai_compact_mode":      service.OpenAICompactModeForceOff,
			"openai_compact_supported": true,
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.Equal(t, service.OpenAICompactModeForceOff, got.GetOpenAICompactMode())
	supported, known := got.OpenAICompactSupportKnown()
	require.True(t, known)
	require.False(t, supported, "force_off must remain authoritative after snapshot filtering")
}

func TestBuildSchedulerMetadataAccount_KeepsPromptCacheAffinityModeWithoutChildFeatures(t *testing.T) {
	account := service.Account{
		ID:       202,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                                        true,
			"prompt_cache_boost_enabled":                       true,
			"prompt_cache_boost_level":                         service.OpenAIPromptCacheBoostLevelAggressive,
			"prompt_cache_boost_upstream_hit_priority_enabled": true,
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.True(t, got.IsOpenAIPromptCacheBoostEnabled())
	require.True(t, got.IsOpenAIPromptCacheBoostAggressive())
	require.True(t, got.IsOpenAIPromptCacheBoostUpstreamHitPriorityEnabled())
	require.False(t, got.IsOpenAIPromptCacheSmartRoutingEnabled())
}

func TestBuildSchedulerMetadataAccount_KeepsAnthropicCacheAffinityMode(t *testing.T) {
	account := service.Account{
		ID:       203,
		Platform: service.PlatformAnthropic,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                                           true,
			"anthropic_cache_boost_enabled":                       true,
			"anthropic_cache_boost_level":                         service.AnthropicCacheBoostLevelAggressive,
			"anthropic_cache_boost_upstream_hit_priority_enabled": true,
			"anthropic_upstream_strong_isolation_enabled":         true,
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.True(t, got.IsAnthropicCacheBoostEnabled())
	require.True(t, got.IsAnthropicCacheBoostAggressive())
	require.True(t, got.IsAnthropicCacheBoostUpstreamHitPriorityEnabled())
	require.True(t, got.IsAnthropicUpstreamStrongIsolationEnabled())
}

func TestSchedulerCacheWriteAccountsDeletesStaleUnencodableAccount(t *testing.T) {
	miniRedis := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: miniRedis.Addr()})
	defer func() { _ = client.Close() }()

	cache := &schedulerCache{rdb: client, writeChunkSize: 16}
	accountID := int64(302)
	accountKey := schedulerAccountKey("302")
	metaKey := schedulerAccountMetaKey("302")
	require.NoError(t, client.Set(context.Background(), accountKey, "stale", 0).Err())
	require.NoError(t, client.Set(context.Background(), metaKey, "stale", 0).Err())

	accounts, err := cache.writeAccounts(context.Background(), []service.Account{{
		ID: accountID,
		Extra: map[string]any{
			"invalid": func() {},
		},
	}})
	require.NoError(t, err)
	require.Empty(t, accounts)
	_, err = client.Get(context.Background(), accountKey).Result()
	require.ErrorIs(t, err, redis.Nil)
	_, err = client.Get(context.Background(), metaKey).Result()
	require.ErrorIs(t, err, redis.Nil)
}
