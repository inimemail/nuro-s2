package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func pricingServiceWithOverride(t *testing.T, body string) *PricingService {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pricing-overrides.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	cfg := &config.Config{}
	cfg.Pricing.OverrideFile = path
	return &PricingService{cfg: cfg}
}

func TestPricingOverrideSparseMergeKeepsCatalogFields(t *testing.T) {
	svc := pricingServiceWithOverride(t, `{"gpt-test":{"input_cost_per_token":0.000003}}`)
	data, err := svc.parsePricingData([]byte(`{"gpt-test":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000009,"litellm_provider":"openai","mode":"chat"}}`))
	require.NoError(t, err)
	require.InDelta(t, 0.000003, data["gpt-test"].InputCostPerToken, 1e-12)
	require.InDelta(t, 0.000009, data["gpt-test"].OutputCostPerToken, 1e-12)
	require.Equal(t, "openai", data["gpt-test"].LiteLLMProvider)
}

func TestPricingOverrideRejectsEntireFileOnNegativePrice(t *testing.T) {
	svc := pricingServiceWithOverride(t, `{"gpt-test":{"input_cost_per_token":-1},"other":{"output_cost_per_token":0.5}}`)
	data, err := svc.parsePricingData([]byte(`{"gpt-test":{"input_cost_per_token":0.000001},"other":{"output_cost_per_token":0.000002}}`))
	require.NoError(t, err)
	require.InDelta(t, 0.000001, data["gpt-test"].InputCostPerToken, 1e-12)
	require.InDelta(t, 0.000002, data["other"].OutputCostPerToken, 1e-12)
}

func TestPricingOverrideRejectsEntireFileOnWrongFieldType(t *testing.T) {
	svc := pricingServiceWithOverride(t, `{"gpt-test":{"supports_prompt_caching":"yes"}}`)
	data, err := svc.parsePricingData([]byte(`{"gpt-test":{"input_cost_per_token":0.000001,"supports_prompt_caching":true}}`))
	require.NoError(t, err)
	require.True(t, data["gpt-test"].SupportsPromptCaching)
}

func TestPricingOverrideCanAddFullyPricedModel(t *testing.T) {
	svc := pricingServiceWithOverride(t, `{"local-model":{"input_cost_per_token":0.000004,"output_cost_per_token":0.000008,"litellm_provider":"local","mode":"chat"}}`)
	data := svc.mergeOverrideOnlyModels(map[string]*LiteLLMModelPricing{})
	require.Contains(t, data, "local-model")
	require.InDelta(t, 0.000004, data["local-model"].InputCostPerToken, 1e-12)
}

func TestPricingOverrideMarksModelForBillingProtection(t *testing.T) {
	svc := pricingServiceWithOverride(t, `{"deepseek-v4-pro":{"input_cost_per_token":0.000009,"output_cost_per_token":0.000017}}`)
	data, err := svc.parsePricingData([]byte(`{"deepseek-v4-pro":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}`))
	require.NoError(t, err)
	require.True(t, data["deepseek-v4-pro"].PricingOverride)
	svc.pricingData = data

	billing := NewBillingService(&config.Config{}, svc)
	pricing, err := billing.GetModelPricing("deepseek-v4-pro")
	require.NoError(t, err)
	require.InDelta(t, 0.000009, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, 0.000017, pricing.OutputPricePerToken, 1e-12)
}

func TestPricingOverrideProtectsOnlyExplicitDeepSeekPriceFields(t *testing.T) {
	svc := pricingServiceWithOverride(t, `{"deepseek-v4-pro":{"input_cost_per_token":0.000009,"litellm_provider":"custom"}}`)
	data, err := svc.parsePricingData([]byte(`{"deepseek-v4-pro":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002,"cache_read_input_token_cost":0.0000001,"litellm_provider":"deepseek"}}`))
	require.NoError(t, err)
	require.True(t, data["deepseek-v4-pro"].PricingOverrideInput)
	require.False(t, data["deepseek-v4-pro"].PricingOverrideOutput)
	require.False(t, data["deepseek-v4-pro"].PricingOverrideCacheRead)
	svc.pricingData = data

	billing := NewBillingService(&config.Config{}, svc)
	pricing, err := billing.GetModelPricing("deepseek-v4-pro")
	require.NoError(t, err)
	require.InDelta(t, 0.000009, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, deepseekProOffPeakOutputPrice, pricing.OutputPricePerToken, 1e-12)
	require.InDelta(t, deepseekProOffPeakCacheRead, pricing.CacheReadPricePerToken, 1e-12)
}

func TestPricingMetadataOverrideDoesNotDisableDeepSeekOfficialPricing(t *testing.T) {
	svc := pricingServiceWithOverride(t, `{"deepseek-v4-pro":{"litellm_provider":"custom"}}`)
	data, err := svc.parsePricingData([]byte(`{"deepseek-v4-pro":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002,"litellm_provider":"deepseek"}}`))
	require.NoError(t, err)
	require.False(t, data["deepseek-v4-pro"].PricingOverride)
	svc.pricingData = data

	billing := NewBillingService(&config.Config{}, svc)
	pricing, err := billing.GetModelPricing("deepseek-v4-pro")
	require.NoError(t, err)
	require.InDelta(t, deepseekProOffPeakInputPrice, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, deepseekProOffPeakOutputPrice, pricing.OutputPricePerToken, 1e-12)
}
