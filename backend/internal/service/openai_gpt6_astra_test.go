package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGPT6AstraAliasesAndFallbackPricing(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "openai/gpt-6-astra", "gpt-6", "openai/gpt-6"} {
		require.Equal(t, "gpt-6-astra", normalizeKnownOpenAICodexModel(model))
		require.True(t, isOpenAIGPT6AstraModel(model))
	}
	require.False(t, isOpenAIGPT56Model("gpt-6-astra"), "A/B/C/D cache creation remains GPT-5.6-only")

	billing := NewBillingService(&config.Config{}, nil)
	pricing, err := billing.GetModelPricing("gpt-6")
	require.NoError(t, err)
	require.InDelta(t, 10e-6, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, 50e-6, pricing.OutputPricePerToken, 1e-12)
	require.InDelta(t, 1e-6, pricing.CacheReadPricePerToken, 1e-12)
	require.Equal(t, openAIGPT54LongContextInputThreshold, pricing.LongContextInputThreshold)

	remoteFallback := (&PricingService{pricingData: map[string]*LiteLLMModelPricing{}}).GetModelPricing("gpt-6")
	require.NotNil(t, remoteFallback)
	require.InDelta(t, 10e-6, remoteFallback.InputCostPerToken, 1e-12)
	require.True(t, remoteFallback.SupportsServiceTier)
}

func TestGPT6AstraUsesExistingDownstreamCacheMarkupButNotCacheCreationPresentation(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"openai_downstream_cache_markup_enabled":            true,
		"openai_downstream_cache_markup_threshold_tokens":   100000,
		"openai_downstream_cache_markup_percent_bps":        1000,
		"openai_prompt_cache_creation_optimization_enabled": true,
		"openai_prompt_cache_creation_optimization_mode":    OpenAIPromptCacheCreationOptimizationModeFree,
	}}
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil)}

	require.True(t, isOpenAIGPTTextModel("gpt-6-astra"))
	require.Empty(t, openAIDownstreamCacheUsageMode(account, "gpt-6-astra"))

	policy := svc.openAIDownstreamCacheMarkupPolicyForContext(context.Background(), account, "gpt-6-astra")
	require.True(t, policy.enabled())
	require.Equal(t, int64(100000), policy.ThresholdTokens)
	require.Equal(t, int64(1000), policy.PercentBPS)

	body := []byte(`{"usage":{"input_tokens":130000,"output_tokens":1000,"total_tokens":131000,"input_tokens_details":{"cached_tokens":120000}}}`)
	updated, changed := normalizeOpenAIDownstreamUsageJSONWithMarkup(body, "", policy)
	require.True(t, changed)
	require.NotEqual(t, body, updated)
}
