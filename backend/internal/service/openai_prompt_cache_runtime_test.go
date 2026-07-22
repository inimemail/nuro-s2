package service

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOpenAIPromptCacheCreationUnsupportedStateIsSharedAcrossServices(t *testing.T) {
	miniRedis := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: miniRedis.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"openai_prompt_cache_creation_optimization_enabled": true,
		},
	}
	first := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	second := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	first.RecordOpenAIPromptCacheCreationOptimizationUnsupported(account)

	require.False(t, second.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account))
	updated, result, err := second.ApplyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", []byte(`{"model":"gpt-5.6-sol"}`))
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.JSONEq(t, `{"model":"gpt-5.6-sol"}`, string(updated))
}

func TestOpenAIPromptCacheBoostUnsupportedStateIsSharedAcrossServices(t *testing.T) {
	miniRedis := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: miniRedis.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	account := &Account{
		ID:       43,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"prompt_cache_boost_enabled": true,
			"pool_mode":                  true,
		},
	}
	first := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	second := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	first.temporarilyDisableOpenAIPromptCacheBoost(account, true, false)

	require.False(t, second.isOpenAIPromptCacheBoostKeyRuntimeEnabled(account))
	require.True(t, second.isOpenAIPromptCacheBoostRetentionRuntimeEnabled(account))
}

func TestOpenAIPromptCacheCreationRuntimeSkipsRedisForInapplicableRequests(t *testing.T) {
	miniRedis := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: miniRedis.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	account := &Account{
		ID:       44,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"openai_prompt_cache_creation_optimization_enabled": true,
		},
	}
	svc := &OpenAIGatewayService{openaiAccountHealthRedis: client}

	_, result, err := svc.ApplyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.5", []byte(`{"model":"gpt-5.5"}`))
	require.NoError(t, err)
	require.False(t, result.Applied)
	_, checked := svc.openaiPromptCacheCreationRemoteCheckedAt.Load(account.ID)
	require.False(t, checked)

	_, result, err = svc.ApplyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(account, "gpt-5.6-sol", []byte(`{"model":"gpt-5.6-sol"}`), true)
	require.NoError(t, err)
	require.False(t, result.Applied)
	_, checked = svc.openaiPromptCacheCreationRemoteCheckedAt.Load(account.ID)
	require.False(t, checked)
}
