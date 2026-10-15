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
	scope := OpenAIPromptCacheCreationOptimizationScope("gpt-5.6-sol", []byte(`{"model":"gpt-5.6-sol"}`))
	first.RecordOpenAIPromptCacheCreationOptimizationUnsupported(account, scope)

	require.False(t, second.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account, scope))
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

	_, result, err := svc.ApplyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-image-1", []byte(`{"model":"gpt-image-1"}`))
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.Empty(t, svc.openaiPromptCacheCreationRuntime)

	_, result, err = svc.ApplyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(account, "gpt-5.6-sol", []byte(`{"model":"gpt-5.6-sol"}`), true)
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.Empty(t, svc.openaiPromptCacheCreationRuntime)
}

func TestOpenAIPromptCacheCreationRuntimeBackoffIsModelAndProtocolScoped(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	account.ID = 45
	first := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	second := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	responses := []byte(`{"model":"provider-text-alias","input":"hello"}`)
	chat := []byte(`{"model":"provider-text-alias","messages":[{"role":"user","content":"hello"}]}`)
	first.RecordOpenAIPromptCacheCreationOptimizationUnsupported(account, OpenAIPromptCacheCreationOptimizationScope("provider-text-alias", responses))
	_, result, err := second.ApplyOpenAIPromptCacheCreationOptimizationBody(account, "provider-text-alias", responses)
	require.NoError(t, err)
	require.False(t, result.Applied)
	_, result, err = second.ApplyOpenAIPromptCacheCreationOptimizationBody(account, "provider-text-alias", chat)
	require.NoError(t, err)
	require.True(t, result.Applied, "a rejection on Responses must not disable Chat Completions")
	_, result, err = second.ApplyOpenAIPromptCacheCreationOptimizationBody(account, "another-text-alias", responses)
	require.NoError(t, err)
	require.True(t, result.Applied, "a rejection for one model must not disable its peers")
	mini.FastForward(openAIPromptCacheCreationUnsupportedDisableTTL)
	third := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	_, result, err = third.ApplyOpenAIPromptCacheCreationOptimizationBody(account, "provider-text-alias", responses)
	require.NoError(t, err)
	require.True(t, result.Applied)
}
