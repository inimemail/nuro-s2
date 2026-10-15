package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAICacheCreationWSEdgePlanUsesPerTurnSettlementForDOnly(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: promptCacheBoostTestConfig()}
	svc.cfg.Gateway.OpenAIWS.Enabled = true
	svc.cfg.Gateway.OpenAIWS.OAuthEnabled = true
	svc.cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	svc.cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	svc.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	svc.cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, model := range []string{"gpt-5.6-sol", "custom-text", "gpt-4o-realtime-preview"} {
			for _, mode := range []string{"reduce", "suppress", "free", "input_125", "disabled"} {
				t.Run(accountType+"/"+model+"/"+mode, func(t *testing.T) {
					account := promptCacheCreationOptimizationAccount(accountType, mode != "disabled", mode)
					account.Concurrency = 1
					account.Extra = map[string]any{
						"openai_oauth_responses_websockets_v2_mode":  OpenAIWSIngressModePassthrough,
						"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough,
					}
					body := []byte(`{"type":"response.create","model":"` + model + `","input":"hello"}`)
					plan, err := svc.BuildResponsesWSEdgePlan(context.Background(), nil, account, body, "test-token")
					if mode == "input_125" {
						require.EqualError(t, err, "cache_creation_pricing_requires_go_ws")
						require.Nil(t, plan, "D must use Go's per-turn settlement before opening an Edge upstream")
					} else {
						require.NoError(t, err, "A/B/C and disabled accounts must retain the Edge path")
						require.NotNil(t, plan)
					}
				})
			}
		}
	}
}

func TestOpenAICacheCreationWSPresentationPerTurn(t *testing.T) {
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"custom-text": {InputCostPerToken: 2e-6, OutputCostPerToken: 10e-6, CacheReadInputTokenCost: .2e-6, CacheCreationInputTokenCost: 3e-6,
			InputCostPerTokenPriority: 4e-6, OutputCostPerTokenPriority: 25e-6, CacheReadInputTokenCostPriority: .4e-6, CacheCreationInputTokenCostPriority: 7e-6},
	}})}
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	body := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":1300,"output_tokens":200,"input_tokens_details":{"cached_tokens":200,"cache_write_tokens":1000}}}}`)
	meta := newOpenAIWSPassthroughUsageMeta("custom-text", nil)
	meta.initFromFirstFrame([]byte(`{"type":"response.create","model":"custom-text","service_tier":"priority"}`))
	updated := svc.normalizeOpenAIWSPassthroughDownstreamUsage(context.Background(), account, meta, body)
	require.Equal(t, int64(2033), gjson.GetBytes(updated, "response.usage.input_tokens").Int())
	require.Zero(t, gjson.GetBytes(updated, "response.usage.input_tokens_details.cache_write_tokens").Int())
	require.Equal(t, int64(1000), gjson.GetBytes(body, "response.usage.input_tokens_details.cache_write_tokens").Int(), "upstream audit usage must remain real")

	// An image turn must be byte-exact; the next text turn must not inherit it.
	meta.explicitImageIntent.Store(true)
	require.Equal(t, body, svc.normalizeOpenAIWSPassthroughDownstreamUsage(context.Background(), account, meta, body))
	meta.explicitImageIntent.Store(false)
	meta.updateFromResponseCreate([]byte(`{"type":"response.create","model":"custom-text"}`), "custom-text")
	updated = svc.normalizeOpenAIWSPassthroughDownstreamUsage(WithOpenAIImageGenerationIntent(context.Background()), account, meta, body)
	require.Equal(t, int64(1795), gjson.GetBytes(updated, "response.usage.input_tokens").Int())

	meta.updateFromResponseCreate([]byte(`{"type":"response.create","model":"unpriced-alias"}`), "unpriced-alias")
	require.Equal(t, body, svc.normalizeOpenAIWSPassthroughDownstreamUsage(context.Background(), account, meta, body))
	account.Credentials["openai_prompt_cache_creation_optimization_mode"] = OpenAIPromptCacheCreationOptimizationModeFree
	updated = svc.normalizeOpenAIWSPassthroughDownstreamUsage(context.Background(), account, meta, body)
	require.Equal(t, int64(300), gjson.GetBytes(updated, "response.usage.input_tokens").Int())
	account.Credentials["openai_prompt_cache_creation_optimization_enabled"] = false
	require.Equal(t, body, svc.normalizeOpenAIWSPassthroughDownstreamUsage(context.Background(), account, meta, body))
}

func TestOpenAICacheCreationRuntimeLocalScopesAndBoundedState(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	account.ID = 88
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"custom-text"}`)
	scope := OpenAIPromptCacheCreationOptimizationScope("custom-text", body)
	svc.RecordOpenAIPromptCacheCreationOptimizationUnsupported(account, scope)
	require.False(t, svc.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account, scope))
	require.True(t, svc.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account, OpenAIPromptCacheCreationOptimizationScope("other-text", body)))
	require.True(t, svc.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account, OpenAIPromptCacheCreationOptimizationScope("custom-text", []byte(`{"type":"response.create"}`))))
	for i := 0; i < openAIPromptCacheCreationMaxRuntimeStates+20; i++ {
		svc.RecordOpenAIPromptCacheCreationOptimizationUnsupported(account, fmt.Sprint(i))
	}
	require.LessOrEqual(t, len(svc.openaiPromptCacheCreationRuntime), openAIPromptCacheCreationMaxRuntimeStates)
	require.False(t, svc.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account, "uncached-full"))
	svc.openaiPromptCacheCreationRuntimeMu.Lock()
	for key := range svc.openaiPromptCacheCreationRuntime {
		svc.openaiPromptCacheCreationRuntime[key] = openAIPromptCacheCreationRuntimeState{disabledUntil: time.Now().Add(-time.Second)}
	}
	svc.openaiPromptCacheCreationRuntimeCleanupAt = time.Time{}
	svc.openaiPromptCacheCreationRuntimeMu.Unlock()
	require.True(t, svc.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account, "new-scope"))
	require.Len(t, svc.openaiPromptCacheCreationRuntime, 1)
}

func TestOpenAICacheCreationRuntimeConcurrentLocalAccess(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	account.ID = 89
	svc := &OpenAIGatewayService{}
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			for j := 0; j < 50; j++ {
				scope := fmt.Sprint(i % 4)
				svc.RecordOpenAIPromptCacheCreationOptimizationUnsupported(account, scope)
				svc.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account, scope)
			}
		}(i)
	}
	workers.Wait()
	require.Len(t, svc.openaiPromptCacheCreationRuntime, 4)
}
