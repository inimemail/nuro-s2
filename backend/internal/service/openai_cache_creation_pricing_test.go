package service

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAICacheCreationPricing_SharedGoRustCases(t *testing.T) {
	data, err := os.ReadFile("testdata/openai_cache_creation_pricing_cases.json")
	require.NoError(t, err)
	var cases []struct {
		Name                             string
		Ordinary, Read, Creation, Output int64
		AllowOutput                      bool `json:"allow_output"`
		Tier                             string
		Policy                           *OpenAICacheCreationPricingPolicy
		Applied                          bool
		Want                             [4]int64
	}
	require.NoError(t, json.Unmarshal(data, &cases))
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			allocation, applied := allocateOpenAICacheCreationPricedUsage(tc.Ordinary, tc.Read, tc.Creation, tc.Output, tc.AllowOutput, tc.Tier, tc.Policy)
			require.Equal(t, tc.Applied, applied)
			require.Equal(t, tc.Want, [4]int64{allocation.ordinary, allocation.cacheRead, allocation.creation, allocation.output})
		})
	}
}

func TestOpenAICacheCreationPricing_PreservesEffectiveModelCostAndUpstreamUsage(t *testing.T) {
	billing := NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{"provider-text-alias": {
		InputCostPerToken: 2e-6, OutputCostPerToken: 10e-6, CacheReadInputTokenCost: .2e-6, CacheCreationInputTokenCost: 3e-6,
		InputCostPerTokenPriority: 4e-6, OutputCostPerTokenPriority: 25e-6, CacheReadInputTokenCostPriority: .4e-6, CacheCreationInputTokenCostPriority: 7e-6,
	}}})
	// An explicit pricing override can identify a custom provider alias.
	group := &Group{ID: 1, Hydrated: true, Platform: PlatformOpenAI, Status: StatusActive, LongContextPricingEnabled: true, ModelPricing: []ChannelModelPricing{{Models: []string{"provider-text-alias"}, InputPrice: cacheCreationPricePtr(2e-6), OutputPrice: cacheCreationPricePtr(10e-6), CacheReadPrice: cacheCreationPricePtr(.2e-6), CacheWritePrice: cacheCreationPricePtr(3e-6)}}}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	svc := &OpenAIGatewayService{billingService: billing}
	for _, ctx := range []context.Context{context.Background(), ctx} {
		for _, tier := range []string{"default", "priority"} {
			t.Run(tier, func(t *testing.T) {
				body := []byte(`{"service_tier":"` + tier + `","usage":{"input_tokens":1300,"output_tokens":200,"total_tokens":1500,"input_tokens_details":{"cached_tokens":200,"cache_write_tokens":1000}}}`)
				updated, changed := svc.normalizeOpenAIDownstreamUsageForRequest(body, ctx, account, "provider-text-alias")
				require.True(t, changed)
				parsed, ok := extractOpenAIUsageFromJSONBytes(updated)
				require.True(t, ok)
				require.Zero(t, parsed.CacheCreationInputTokens)
				resolver := NewModelPricingResolver(nil, billing)
				input := PricingInput{Model: "provider-text-alias"}
				if g, ok := ctx.Value(ctxkey.Group).(*Group); ok {
					input.Group = g
					input.GroupID = &g.ID
				}
				pricing := resolver.Resolve(ctx, input)
				actual := billing.computeTokenBreakdown(pricing.BasePricing, UsageTokens{InputTokens: 100, CacheReadTokens: 200, CacheCreationTokens: 1000, OutputTokens: 200}, 1, tier, true)
				displayed := billing.computeTokenBreakdown(pricing.BasePricing, UsageTokens{InputTokens: parsed.InputTokens - parsed.CacheReadInputTokens, CacheReadTokens: parsed.CacheReadInputTokens, OutputTokens: parsed.OutputTokens}, 1, tier, true)
				inputPrice := pricing.BasePricing.InputPricePerToken
				if usePriorityServiceTierPricing(tier, pricing.BasePricing) && pricing.BasePricing.InputPricePerTokenPriority > 0 {
					inputPrice = pricing.BasePricing.InputPricePerTokenPriority
				}
				require.InDelta(t, actual.TotalCost, displayed.TotalCost, inputPrice/2+1e-12, "rounding must stay within half an ordinary-input token")
				require.Equal(t, int64(1000), gjson.GetBytes(body, "usage.input_tokens_details.cache_write_tokens").Int())
				typed := &apicompat.ResponsesUsage{InputTokens: 1300, OutputTokens: 200, TotalTokens: 1500, CacheCreationInputTokens: 1000, InputTokensDetails: &apicompat.ResponsesInputTokensDetails{CachedTokens: 200, CacheWriteTokens: 1000}}
				require.True(t, normalizeOpenAIResponsesUsageForDownstreamWithMarkup(typed, OpenAIPromptCacheCreationOptimizationModeInput125, svc.openAIDownstreamCacheMarkupPolicyForContext(ctx, account, "provider-text-alias"), tier))
				require.Equal(t, parsed.InputTokens, typed.InputTokens)
				require.Equal(t, parsed.OutputTokens, typed.OutputTokens)
			})
		}
	}
}

func TestOpenAICacheCreationPricing_UnknownDRemainsRawAndCKeepsWorking(t *testing.T) {
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil)}
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	body := []byte(`{ "usage": {"input_tokens":1300,"output_tokens":200,"input_tokens_details":{"cached_tokens":200,"cache_write_tokens":1000}} }`)
	updated, changed := svc.normalizeOpenAIDownstreamUsageForRequest(body, context.Background(), account, "custom-provider-alias")
	require.False(t, changed)
	require.Equal(t, body, updated)
	account.Credentials["openai_prompt_cache_creation_optimization_mode"] = OpenAIPromptCacheCreationOptimizationModeFree
	updated, changed = svc.normalizeOpenAIDownstreamUsageForRequest(body, context.Background(), account, "custom-provider-alias")
	require.True(t, changed)
	require.Zero(t, gjson.GetBytes(updated, "usage.input_tokens_details.cache_write_tokens").Int())
	require.Equal(t, int64(300), gjson.GetBytes(updated, "usage.input_tokens").Int())
}

func TestOpenAICacheCreationPricing_RejectsInvalidPricesAndOverflow(t *testing.T) {
	for _, price := range []float64{math.NaN(), math.Inf(1), -1, 1e10, 1e-30} {
		require.Nil(t, openAICacheCreationPricingSnapshot(&ModelPricing{InputPricePerToken: 1, OutputPricePerToken: 1, CacheCreationPricePerToken: price}, false))
	}
	policy := &OpenAICacheCreationPricingPolicy{Rates: OpenAICacheCreationRates{Input: 1, Read: 1, Creation: math.MaxInt64, Output: 1}}
	_, applied := allocateOpenAICacheCreationPricedUsage(1, 1, math.MaxInt64, 1, true, "", policy)
	require.False(t, applied)
}

func cacheCreationPricePtr(price float64) *float64 { return &price }

func TestOpenAICacheCreationPricing_UsesChannelBillingModelSource(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	group := &Group{ID: 7, Hydrated: true, Platform: PlatformOpenAI, Status: StatusActive, LongContextPricingEnabled: true}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	for _, tc := range []struct{ source, mapped, want string }{
		{BillingModelSourceRequested, "channel-alias", "requested-alias"},
		{BillingModelSourceChannelMapped, "channel-alias", "channel-alias"},
		{BillingModelSourceChannelMapped, "", "upstream-alias"},
		{BillingModelSourceChannelMapped, "requested-alias", "upstream-alias"},
		{BillingModelSourceUpstream, "channel-alias", "upstream-alias"},
	} {
		t.Run(tc.source+"/"+tc.mapped, func(t *testing.T) {
			channel := Channel{ID: 2, Status: StatusActive, GroupIDs: []int64{group.ID}, BillingModelSource: tc.source,
				ModelMapping: map[string]map[string]string{PlatformOpenAI: {}},
				ModelPricing: []ChannelModelPricing{{Platform: PlatformOpenAI, Models: []string{tc.want}, InputPrice: cacheCreationPricePtr(2e-6), OutputPrice: cacheCreationPricePtr(10e-6), CacheReadPrice: cacheCreationPricePtr(.2e-6), CacheWritePrice: cacheCreationPricePtr(3e-6)}}}
			if tc.mapped != "" {
				channel.ModelMapping[PlatformOpenAI]["requested-alias"] = tc.mapped
			}
			channels := &ChannelService{}
			channels.cache.Store(populateChannelCache([]Channel{channel}, map[int64]string{group.ID: PlatformOpenAI}))
			svc := &OpenAIGatewayService{channelService: channels, billingService: NewBillingService(&config.Config{}, nil)}
			require.Equal(t, tc.want, svc.openAICacheCreationPresentationModel(ctx, "upstream-alias", "requested-alias"))
			policy := svc.openAIDownstreamCacheMarkupPolicyForContext(ctx, account, "upstream-alias", "requested-alias")
			require.True(t, policy.CreationPricingRequired)
			require.NotNil(t, policy.CreationPricing)
			require.Equal(t, int64(3e-6*openAIDownstreamCacheMarkupPriceScale), policy.CreationPricing.Rates.Creation)
		})
	}
}

func TestOpenAICacheCreationPricing_RespectsAccountLongContextSwitch(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil)}
	policy := svc.openAICacheCreationPricingForContext(context.Background(), account, "gpt-5.6-terra")
	require.NotNil(t, policy)
	require.Zero(t, policy.LongContextThreshold)
	account.Extra = map[string]any{openAILongContextBillingEnabledKey: true}
	policy = svc.openAICacheCreationPricingForContext(context.Background(), account, "gpt-5.6-terra")
	require.NotNil(t, policy)
	require.Equal(t, int64(272000), policy.LongContextThreshold)
}

func TestOpenAICacheCreationPricing_MissingResponseTierKeepsRequestTier(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"gpt-pricing-alias": {InputCostPerToken: 2e-6, OutputCostPerToken: 10e-6, CacheReadInputTokenCost: .2e-6, CacheCreationInputTokenCost: 3e-6,
			InputCostPerTokenPriority: 4e-6, OutputCostPerTokenPriority: 25e-6, CacheReadInputTokenCostPriority: .4e-6, CacheCreationInputTokenCostPriority: 7e-6},
	}})}
	group := &Group{ID: 7}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	ctx = withOpenAICacheCreationServiceTier(ctx, account, "priority")
	require.Same(t, group, ctx.Value(ctxkey.Group))
	for _, markupEnabled := range []bool{false, true} {
		account.Credentials["openai_downstream_cache_markup_enabled"] = markupEnabled
		account.Credentials["openai_downstream_cache_markup_percent_bps"] = 1000
		account.Credentials["openai_downstream_cache_markup_threshold_tokens"] = 1000000
		policy := svc.openAIDownstreamCacheMarkupPolicyForContext(ctx, account, "gpt-pricing-alias")
		require.Equal(t, markupEnabled, policy.enabled())
		require.Equal(t, "priority", policy.CreationServiceTier)
		body := []byte(`{"usage":{"input_tokens":1300,"output_tokens":200,"input_tokens_details":{"cached_tokens":200,"cache_write_tokens":1000}}}`)
		updated, changed := svc.normalizeOpenAIDownstreamUsageForRequest(body, ctx, account, "gpt-pricing-alias")
		require.True(t, changed)
		require.Equal(t, int64(2033), gjson.GetBytes(updated, "usage.input_tokens").Int())
		usage := &apicompat.ResponsesUsage{InputTokens: 1300, OutputTokens: 200, CacheCreationInputTokens: 1000, InputTokensDetails: &apicompat.ResponsesInputTokensDetails{CachedTokens: 200, CacheWriteTokens: 1000}}
		event := &apicompat.ResponsesStreamEvent{Usage: usage, Response: &apicompat.ResponsesResponse{Usage: usage}}
		require.True(t, normalizeOpenAIResponsesStreamEventForDownstreamWithMarkup(event, OpenAIPromptCacheCreationOptimizationModeInput125, policy, ""))
		require.Equal(t, 2033, event.Usage.InputTokens)
		require.Equal(t, 2033, event.Response.Usage.InputTokens)
		require.Equal(t, 1300, usage.InputTokens, "billing must retain upstream usage")
		buffered := cloneOpenAIResponsesUsage(usage)
		require.True(t, normalizeOpenAIResponsesUsageForDownstreamWithMarkup(buffered, OpenAIPromptCacheCreationOptimizationModeInput125, policy, ""))
		require.Equal(t, 2033, buffered.InputTokens)
	}
	// A later request with no tier cannot inherit a previous priority request.
	ctx = withOpenAICacheCreationServiceTier(ctx, account, "")
	require.Equal(t, "default", svc.openAIDownstreamCacheMarkupPolicyForContext(ctx, account, "gpt-pricing-alias").CreationServiceTier)
	for _, mode := range []string{"reduce", "suppress", "free"} {
		account.Credentials["openai_prompt_cache_creation_optimization_mode"] = mode
		require.Same(t, ctx, withOpenAICacheCreationServiceTier(ctx, account, "priority"))
	}
	account.Credentials["openai_prompt_cache_creation_optimization_enabled"] = false
	require.Same(t, ctx, withOpenAICacheCreationServiceTier(ctx, account, "priority"))
}

func TestOpenAICacheCreationPricing_ResponseTierCannotChangeSettlementPrice(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"custom-text": {InputCostPerToken: 2e-6, OutputCostPerToken: 10e-6, CacheReadInputTokenCost: .2e-6, CacheCreationInputTokenCost: 3e-6,
			InputCostPerTokenPriority: 4e-6, OutputCostPerTokenPriority: 25e-6, CacheReadInputTokenCostPriority: .4e-6, CacheCreationInputTokenCostPriority: 7e-6},
	}})}
	for _, tc := range []struct {
		request, response string
		want              int
	}{
		{"priority", "default", 2033},
		{"", "priority", 1795},
		{"default", "priority", 1795},
	} {
		t.Run(tc.request+"/"+tc.response, func(t *testing.T) {
			ctx := withOpenAICacheCreationServiceTier(context.Background(), account, tc.request)
			policy := svc.openAIDownstreamCacheMarkupPolicyForContext(ctx, account, "custom-text")
			body := []byte(`{"service_tier":"` + tc.response + `","usage":{"input_tokens":1300,"output_tokens":200,"input_tokens_details":{"cached_tokens":200,"cache_write_tokens":1000}}}`)
			updated, changed := svc.normalizeOpenAIDownstreamUsageForRequest(body, ctx, account, "custom-text")
			require.True(t, changed)
			require.Equal(t, int64(tc.want), gjson.GetBytes(updated, "usage.input_tokens").Int())
			usage := &apicompat.ResponsesUsage{InputTokens: 1300, OutputTokens: 200, CacheCreationInputTokens: 1000, InputTokensDetails: &apicompat.ResponsesInputTokensDetails{CachedTokens: 200, CacheWriteTokens: 1000}}
			event := &apicompat.ResponsesStreamEvent{Response: &apicompat.ResponsesResponse{Usage: usage, UpstreamServiceTier: tc.response}}
			require.True(t, normalizeOpenAIResponsesStreamEventForDownstreamWithMarkup(event, OpenAIPromptCacheCreationOptimizationModeInput125, policy))
			require.Equal(t, tc.want, event.Response.Usage.InputTokens)
			require.Equal(t, 1000, usage.CacheCreationInputTokens, "the billing usage must remain untouched")
		})
	}
}

func TestOpenAICacheCreationPricing_EdgeFastFilterKeepsSettlementTier(t *testing.T) {
	for _, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "responses"}[responses], func(t *testing.T) {
			svc := newOpenAIGatewayServiceWithSettings(t, openAIFastFilterPriorityPolicy())
			svc.cfg = promptCacheBoostTestConfig()
			svc.billingService = NewBillingService(&config.Config{}, nil)
			account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
			account.Credentials["api_key"] = "sk-test"
			account.Credentials["base_url"] = "https://api.openai.com"
			var prepared *OpenAIEdgePreparedChatCompletions
			var err error
			if responses {
				account.Extra = map[string]any{"openai_passthrough": true, "openai_responses_supported": true}
				prepared, err = svc.BuildRawResponsesEdgePlan(context.Background(), nil, account, []byte(`{"model":"gpt-5.6-sol","service_tier":"priority","stream":true,"input":"hello"}`))
			} else {
				prepared, err = svc.BuildRawChatCompletionsEdgePlan(context.Background(), nil, account, []byte(`{"model":"gpt-5.6-sol","service_tier":"priority","stream":true,"messages":[{"role":"user","content":"hello"}]}`), "")
			}
			require.NoError(t, err)
			require.NotNil(t, prepared.Plan.DownstreamCacheMarkup)
			require.Equal(t, "priority", openAICacheCreationServiceTier(prepared.ServiceTier))
			require.Equal(t, "priority", prepared.Plan.DownstreamCacheMarkup.CreationServiceTier)
			require.False(t, gjson.GetBytes(prepared.Plan.Body, "service_tier").Exists(), "the existing fast filter must still remove the upstream field")
		})
	}
}

func TestOpenAICacheCreationPricing_OAuthLongContextUsesUpstreamNotBillingAlias(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	account.Extra = map[string]any{openAILongContextBillingEnabledKey: true}
	group := &Group{ID: 7, Hydrated: true, Platform: PlatformOpenAI, Status: StatusActive, LongContextPricingEnabled: true}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	channels := &ChannelService{}
	channels.cache.Store(populateChannelCache([]Channel{{ID: 2, Status: StatusActive, GroupIDs: []int64{group.ID}, BillingModelSource: BillingModelSourceRequested}}, map[int64]string{group.ID: PlatformOpenAI}))
	svc := &OpenAIGatewayService{channelService: channels, billingService: NewBillingService(&config.Config{}, nil)}
	policy := svc.openAIDownstreamCacheMarkupPolicyForContext(ctx, account, "gpt-6.1-sol", "gpt-5.6-terra")
	require.NotNil(t, policy.CreationPricing)
	require.Zero(t, policy.CreationPricing.LongContextThreshold, "official OAuth GPT-6.1 rule follows actual upstream model")
	account.Type = AccountTypeAPIKey
	policy = svc.openAIDownstreamCacheMarkupPolicyForContext(ctx, account, "gpt-6.1-sol", "gpt-5.6-terra")
	require.NotNil(t, policy.CreationPricing)
	require.Equal(t, int64(272000), policy.CreationPricing.LongContextThreshold, "API-key long context must remain enabled")
}

func TestOpenAICacheCreationPricing_TimeDependentCatalogKeepsRawUnlessFixed(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	for _, model := range []string{"deepseek-v4-pro", "gemini-3.6-flash"} {
		t.Run(model, func(t *testing.T) {
			for _, fixed := range []bool{false, true} {
				svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
					model: {InputCostPerToken: 2e-6, OutputCostPerToken: 10e-6, CacheReadInputTokenCost: .2e-6, CacheCreationInputTokenCost: 3e-6,
						PricingOverrideInput: fixed, PricingOverrideOutput: fixed, PricingOverrideCacheRead: fixed},
				}})}
				policy := svc.openAICacheCreationPricingForContext(context.Background(), account, model)
				if fixed {
					require.NotNil(t, policy)
				} else {
					require.Nil(t, policy, "a session snapshot must not use a stale time-dependent price")
				}
				group := &Group{ID: 1, Hydrated: true, Platform: PlatformOpenAI, Status: StatusActive,
					ModelPricing: []ChannelModelPricing{{Models: []string{model}, InputPrice: cacheCreationPricePtr(2e-6), OutputPrice: cacheCreationPricePtr(10e-6), CacheReadPrice: cacheCreationPricePtr(.2e-6), CacheWritePrice: cacheCreationPricePtr(3e-6)}}}
				ctx := context.WithValue(context.Background(), ctxkey.Group, group)
				require.NotNil(t, svc.openAICacheCreationPricingForContext(ctx, account, model), "fixed group pricing remains eligible")
			}
		})
	}
}
