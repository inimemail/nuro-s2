package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV0223ModelsAliasesAndRequestValidation(t *testing.T) {
	for _, model := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		for _, suffix := range []string{"", "-low", "-medium", "-high", "-xhigh", "-max", "-openai-compact"} {
			alias := "openai/" + model + suffix
			require.Equal(t, model, normalizeKnownOpenAICodexModel(alias))
			require.Equal(t, model, normalizeCodexModel(alias))
			require.NoError(t, validateGPT61SolRequestBody([]byte(`{"reasoning":{"effort":"high"}}`), alias))
		}
		for _, raw := range []string{`{"reasoning":{"effort":"none"}}`, `{"reasoning_effort":"minimal"}`, `{"thinking":{"type":"disabled"}}`, `{"reasoning":{"effort":"ultra"}}`} {
			require.Error(t, validateGPT61SolRequestBody([]byte(raw), model), raw)
		}
		require.Error(t, validateGPT61SolRequestBody([]byte(`{}`), model+"-none", "mapped-public-alias"))
		require.Error(t, validateGPT61SolRequestBody([]byte(`{}`), model+"-minimal", model))
		raw := []byte(`{"model":"` + model + `","reasoning":{"effort":"max"},"temperature":0.7,"top_p":1,"logprobs":true,"top_logprobs":2,"include":["message.output_text.logprobs","reasoning.encrypted_content"]}`)
		out, err := normalizeGPT6ResponsesSampling(raw)
		require.NoError(t, err)
		for _, key := range []string{"temperature", "top_p", "logprobs", "top_logprobs"} {
			require.False(t, gjson.GetBytes(out, key).Exists())
		}
		require.Equal(t, "reasoning.encrypted_content", gjson.GetBytes(out, "include.0").String())
		require.Equal(t, "max", normalizeOpenAIReasoningEffortForModel("max", model))
		converted, err := apicompat.AnthropicToResponses(&apicompat.AnthropicRequest{Model: model, OutputConfig: &apicompat.AnthropicOutputConfig{Effort: "max"}})
		require.NoError(t, err)
		require.Equal(t, "max", converted.Reasoning.Effort)
		require.True(t, openai.RequiresResponsesForToolCalls(model))
		require.NotEqual(t, openai.CodexBaseInstructionsForModel("gpt-6.1-sol"), openai.CodexBaseInstructionsForModel(model))
	}
	for _, model := range []string{"gpt-6.2-preview", "gpt-6.2-sol-preview", "gpt-6.2-astra-law", "gpt-6.2-sol-foo"} {
		require.Empty(t, openai.GPT62ModelID(model))
		require.Equal(t, model, normalizeCodexModel(model))
	}
}

func TestV0223PublishedPricesAndFallbacksAgree(t *testing.T) {
	raw, err := os.ReadFile("../../resources/model-pricing/model_prices_and_context_window.json")
	require.NoError(t, err)
	catalog := &PricingService{}
	catalog.pricingData, err = catalog.parsePricingData(raw)
	require.NoError(t, err)
	for _, source := range []*PricingService{catalog, nil, &PricingService{pricingData: map[string]*LiteLLMModelPricing{}}} {
		billing := NewBillingService(&config.Config{}, source)
		for _, item := range []struct {
			model string
			rate  float64
		}{{"gpt-6.2-sol", 1}, {"gpt-6.2-astra", 4}} {
			price, err := billing.GetModelPricing(item.model + "-high")
			require.NoError(t, err)
			require.InDelta(t, 2e-6*item.rate, price.InputPricePerToken, 1e-15)
			require.InDelta(t, 10e-6*item.rate, price.OutputPricePerToken, 1e-15)
			require.InDelta(t, .1e-6*item.rate, price.CacheReadPricePerToken, 1e-15)
			require.InDelta(t, 2.5e-6*item.rate, price.CacheCreationPricePerToken, 1e-15)
			require.Equal(t, 272000, price.LongContextInputThreshold)
			require.Equal(t, 2.0, price.LongContextInputMultiplier)
			require.Equal(t, 1.5, price.LongContextOutputMultiplier)
			tokens := UsageTokens{InputTokens: 1000, OutputTokens: 100, CacheCreationTokens: 200, CacheReadTokens: 300}
			base, err := billing.CalculateCostWithServiceTier(item.model, tokens, 1, "")
			require.NoError(t, err)
			for _, tier := range []struct {
				name   string
				factor float64
			}{{"priority", 2}, {"flex", .5}} {
				cost, err := billing.CalculateCostWithServiceTier(item.model, tokens, 1, tier.name)
				require.NoError(t, err)
				require.InDelta(t, base.TotalCost*tier.factor, cost.TotalCost, 1e-12)
			}
		}
	}
}

func TestV0223ManifestProjectionPreservesUpstreamCapabilities(t *testing.T) {
	out := convertOpenAIModelListToCodexManifest([]byte(`{"data":[{"id":"gpt-6.2-sol"},{"id":"gpt-6.2-astra"},{"id":"custom","context_window":77}]}`))
	require.Equal(t, int64(500000), gjson.GetBytes(out, "models.0.context_window").Int())
	require.Equal(t, int64(272000), gjson.GetBytes(out, "models.1.context_window").Int())
	require.Equal(t, int64(872000), gjson.GetBytes(out, "models.1.max_context_window").Int())
	require.NotContains(t, gjson.GetBytes(out, "models.0.supported_reasoning_levels.#.effort").Raw, "ultra", "API-key projection must not advertise rejected API efforts")
	require.Equal(t, int64(77), gjson.GetBytes(out, "models.2.context_window").Int())
	out = convertOpenAIModelListToCodexManifest([]byte(`{"models":[{"slug":"gpt-6.2-sol","context_window":123,"custom":true}]}`))
	require.Equal(t, int64(123), gjson.GetBytes(out, "models.0.context_window").Int())
	require.True(t, gjson.GetBytes(out, "models.0.custom").Bool())
	adjusted, err := adjustAPIKeyCodexModelsManifest([]byte(`{"models":[{"slug":"gpt-6.2-sol","use_responses_lite":true},{"slug":"custom","use_responses_lite":true}]}`))
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(adjusted, "models.0.use_responses_lite").Bool())
	require.True(t, gjson.GetBytes(adjusted, "models.1.use_responses_lite").Bool())
	// Resolve returns fresh pricing with explicit zero group override intact.
	billing := NewBillingService(&config.Config{}, nil)
	zero := 0.0
	group := &Group{ID: 1, Hydrated: true, Platform: PlatformOpenAI, ModelPricing: []ChannelModelPricing{{Models: []string{"gpt-6.2-sol"}, InputPrice: &zero, CacheReadPrice: &zero, CacheWritePrice: &zero, OutputPrice: &zero}}}
	resolved := NewModelPricingResolver(nil, billing).Resolve(context.Background(), PricingInput{Model: "gpt-6.2-sol", Group: group, GroupID: &group.ID})
	require.NotNil(t, resolved)
	require.Zero(t, resolved.BasePricing.InputPricePerToken)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(out, &envelope))
}

func TestV0223RecordUsageOAuthLongContextExemption(t *testing.T) {
	for _, model := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		for _, kind := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
			repo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			t.Cleanup(svc.StopBillingNotifications)
			raw := OpenAIUsage{InputTokens: 300000, OutputTokens: 2000, CacheReadInputTokens: 100000, CacheCreationInputTokens: 2000}
			result := &OpenAIForwardResult{RequestID: model + "_" + kind, Model: model, UpstreamModel: model, Usage: raw}
			require.NoError(t, svc.RecordUsage(t.Context(), &OpenAIRecordUsageInput{
				Result: result, APIKey: &APIKey{ID: 9901}, User: &User{ID: 9902},
				Account: &Account{ID: 9903, Type: kind, Platform: PlatformOpenAI, Extra: map[string]any{openAILongContextBillingEnabledKey: true}},
			}))
			price, err := svc.billingService.GetModelPricing(model)
			require.NoError(t, err)
			inputFactor, outputFactor := 1.0, 1.0
			if kind == AccountTypeAPIKey {
				inputFactor, outputFactor = 2, 1.5
			}
			want := (198000*price.InputPricePerToken+100000*price.CacheReadPricePerToken+2000*price.CacheCreationPricePerToken)*inputFactor + 2000*price.OutputPricePerToken*outputFactor
			require.NotNil(t, repo.lastLog)
			require.InDelta(t, want, repo.lastLog.TotalCost, 1e-12)
			require.Equal(t, kind == AccountTypeAPIKey, repo.lastLog.LongContextBillingApplied)
			require.Equal(t, raw, result.Usage)
		}
	}
}

func TestV0223DDisplayUsesEffectiveModelPrices(t *testing.T) {
	billing := NewBillingService(&config.Config{}, nil)
	svc := &OpenAIGatewayService{billingService: billing}
	for _, model := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		for _, kind := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
			account := &Account{Platform: PlatformOpenAI, Type: kind, Extra: map[string]any{openAILongContextBillingEnabledKey: true}}
			policy := svc.openAICacheCreationPricingForContext(context.Background(), account, model)
			require.NotNil(t, policy)
			longEnabled := kind == AccountTypeAPIKey
			if longEnabled {
				require.EqualValues(t, 272000, policy.LongContextThreshold)
			} else {
				require.Zero(t, policy.LongContextThreshold)
			}
			for _, total := range []int64{271999, 272000, 272001, 300000} {
				for _, tier := range []string{"", "priority", "flex"} {
					ordinary, read, creation, output := total-10000, int64(8000), int64(2000), int64(1000)
					display, _ := allocateOpenAICacheCreationPricedUsage(ordinary, read, creation, output, true, tier, policy)
					cost := func(input, cached, write, out int64) *CostBreakdown {
						result, err := billing.calculateCostWithServiceTierPolicy(model, UsageTokens{InputTokens: int(input), CacheReadTokens: int(cached), CacheCreationTokens: int(write), OutputTokens: int(out)}, 1, tier, longEnabled)
						require.NoError(t, err)
						return result
					}
					original := cost(ordinary, read, creation, output)
					converted := cost(display.ordinary, display.cacheRead, display.creation, display.output)
					require.Equal(t, original.LongContextBillingApplied, converted.LongContextBillingApplied)
					require.InDelta(t, original.TotalCost, converted.TotalCost, 1e-12, "%s/%s/%s/%d", model, kind, tier, total)
				}
			}
		}
	}
}
