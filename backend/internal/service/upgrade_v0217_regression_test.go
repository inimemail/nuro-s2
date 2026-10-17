package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestUpgradeRetirementHonorsUpstreamTypeAndActualTarget(t *testing.T) {
	parent := int64(42)
	for _, tc := range []struct {
		name, kind, requested, target, compact string
		shadow                                 bool
		want                                   bool
	}{
		{name: "oauth retired", kind: AccountTypeOAuth, requested: "gpt-5.5"},
		{name: "setup retired", kind: AccountTypeSetupToken, requested: "gpt-5.5-high"},
		{name: "key retained", kind: AccountTypeAPIKey, requested: "gpt-5.5", want: true},
		{name: "shadow retired", kind: AccountTypeOAuth, requested: "gpt-5.5", shadow: true},
		{name: "passthrough alias ignores ordinary mapping", kind: AccountTypeOAuth, requested: "public", target: "gpt-5.5", want: true},
		{name: "passthrough retired request remains blocked", kind: AccountTypeOAuth, requested: "gpt-5.5", target: "gpt-5.6"},
		{name: "compact mapped retired", kind: AccountTypeOAuth, requested: "gpt-5.6", compact: "gpt-5.5"},
		{name: "compact key retained", kind: AccountTypeAPIKey, requested: "gpt-5.6", compact: "gpt-5.5", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{Platform: PlatformOpenAI, Type: tc.kind, Credentials: map[string]any{}, Extra: map[string]any{"openai_passthrough": true}}
			if tc.shadow {
				a.ParentAccountID = &parent
			}
			if tc.target != "" {
				a.Credentials["model_mapping"] = map[string]any{tc.requested: tc.target}
			}
			if tc.compact != "" {
				a.Credentials["compact_model_mapping"] = map[string]any{tc.requested: tc.compact}
			}
			req := OpenAIAccountScheduleRequest{RequestedModel: tc.requested, RequireCompact: tc.compact != "", RequiredCapability: OpenAIEndpointCapabilityChatCompletions}
			require.Equal(t, tc.want, openAITextRequestModelSupported(a, req))
		})
	}
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.Equal(t, "gpt-5.5", resolveOpenAIAccountUpstreamModelForRequest(a, "gpt-5.6", true, "gpt-5.5"), "resolver must remain authoritative")
	require.False(t, openAITextRequestModelSupported(a, OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.6", RequireCompact: true}, "gpt-5.5"))
}

func TestUpgradeRetirementPassthroughIgnoresOrdinaryMappingForLiveModels(t *testing.T) {
	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"*": "gpt-5.5"},
		},
		Extra: map[string]any{"openai_passthrough": true},
	}
	svc := &OpenAIGatewayService{}
	for _, model := range []string{"gpt-5.6", "gpt-4.1", "gpt-5.4"} {
		require.True(t, openAITextRequestModelSupported(account, OpenAIAccountScheduleRequest{
			RequestedModel:     model,
			RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
		}), model)
		require.True(t, svc.isOpenAIAccountEligibleForRequest(context.Background(), account, model, false, OpenAIEndpointCapabilityChatCompletions, ""), model)
	}
	require.False(t, openAITextRequestModelSupported(account, OpenAIAccountScheduleRequest{
		RequestedModel:     "gpt-5.5",
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
	}))
	require.False(t, svc.isOpenAIAccountEligibleForRequest(context.Background(), account, "gpt-5.5", false, OpenAIEndpointCapabilityChatCompletions, ""))
	require.True(t, svc.isOpenAIAccountEligibleForRequest(context.Background(), account, "gpt-5.6", true, OpenAIEndpointCapabilityChatCompletions, ""), "compact passthrough also ignores ordinary mappings")
	account.Credentials["compact_model_mapping"] = map[string]any{"gpt-5.6": "gpt-5.5"}
	require.False(t, svc.isOpenAIAccountEligibleForRequest(context.Background(), account, "gpt-5.6", true, OpenAIEndpointCapabilityChatCompletions, ""), "compact passthrough checks its actual compact target")
	account.Extra["openai_passthrough"] = false
	require.False(t, svc.isOpenAIAccountEligibleForRequest(context.Background(), account, "gpt-5.6", false, OpenAIEndpointCapabilityChatCompletions, ""), "mapped non-passthrough traffic still targets the retired model")
}

func TestUpgradeRetirementAvailabilityDiagnosisUsesPassthroughPayload(t *testing.T) {
	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"model_mapping": map[string]any{"*": "gpt-5.5"}},
		Extra:       map[string]any{"openai_passthrough": true},
	}
	require.True(t, supportsOpenAIRequestModel(account, "gpt-5.6", false), "availability diagnosis must follow ordinary passthrough payload")
	require.False(t, supportsOpenAIRequestModel(account, "gpt-5.5", false), "retired passthrough payload must remain blocked")
}

func TestUpgradeRetirementLegacyEligibilityUsesCompactFallback(t *testing.T) {
	rateMultiplier := 1.0
	account := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, RateMultiplier: &rateMultiplier,
		Credentials: map[string]any{"access_token": "token"},
		Extra:       map[string]any{"openai_passthrough": true, "openai_compact_supported": true},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAICompactModel = "gpt-5.6"
	svc := &OpenAIGatewayService{cfg: cfg}
	// The legacy helper without service context has no server-wide fallback;
	// the service wrapper below is the production path and supplies it.
	require.False(t, isOpenAIAccountEligibleForRequest(context.Background(), account, "gpt-5.5", true, OpenAIEndpointCapabilityChatCompletions, ""))
	require.True(t, openAITextRequestModelSupported(account, OpenAIAccountScheduleRequest{
		RequestedModel: "gpt-5.5", RequireCompact: true, RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
	}, cfg.Gateway.OpenAICompactModel))
	require.True(t, svc.isOpenAIAccountEligibleForRequest(context.Background(), account, "gpt-5.5", true, OpenAIEndpointCapabilityChatCompletions, ""))
	account.Credentials["compact_model_mapping"] = map[string]any{"gpt-5.5": "gpt-5.5"}
	require.False(t, svc.isOpenAIAccountEligibleForRequest(context.Background(), account, "gpt-5.5", true, OpenAIEndpointCapabilityChatCompletions, ""), "explicit compact mapping takes precedence over the global fallback")
	account.Credentials["compact_model_mapping"] = map[string]any{"gpt-5.5": "gpt-5.6"}
	cfg.Gateway.OpenAICompactModel = "gpt-5.5"
	require.True(t, svc.isOpenAIAccountEligibleForRequest(context.Background(), account, "gpt-5.5", true, OpenAIEndpointCapabilityChatCompletions, ""), "a valid explicit compact target overrides a retired fallback")
	delete(account.Credentials, "compact_model_mapping")
	require.False(t, svc.isOpenAIAccountEligibleForRequest(context.Background(), account, "gpt-5.6", true, OpenAIEndpointCapabilityChatCompletions, ""), "retired actual fallback remains blocked")
}

func TestUpgradeRetirementCatalogProjectionAndMixedPool(t *testing.T) {
	oauth := Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"public": "gpt-5.5", "gpt-5.5": "gpt-5.6"}}}
	body := []byte(`{"models":[{"slug":"public","extra":true},{"slug":"gpt-5.5","extra":"kept"}],"meta":7}`)
	filtered, changed, err := filterRetiredOpenAIPublicModelsBody(body, []Account{oauth}, true)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "gpt-5.5", gjson.GetBytes(filtered, "models.0.slug").String())
	require.Equal(t, "kept", gjson.GetBytes(filtered, "models.0.extra").String())
	require.EqualValues(t, 7, gjson.GetBytes(filtered, "meta").Int())
	key := Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	filtered, changed, err = filterRetiredOpenAIPublicModelsBody(body, []Account{oauth, key}, true)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, filtered)
}

func TestUpgradeRetirementCatalogMappingPrecedence(t *testing.T) {
	account := Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{"model_mapping": map[string]any{
			"*":                   "gpt-5.5",
			"public-*":            "gpt-5.6",
			"public-retired-*":    "gpt-5.5",
			"public-retired-live": "gpt-5.6",
			"gpt-5.5":             "gpt-5.6",
		}},
	}
	models := []string{"public-live", "public-retired-test", "public-retired-live", "other", "gpt-5.5"}
	want := []string{"public-live", "public-retired-live", "gpt-5.5"}
	require.Equal(t, want, filterRetiredOpenAIModelIDs([]Account{account}, models))
	adminModels := make([]openai.Model, 0, len(models))
	for _, model := range models {
		adminModels = append(adminModels, openai.Model{ID: model})
	}
	filtered := FilterRetiredOpenAIAccountModels(&account, adminModels)
	got := make([]string, 0, len(filtered))
	for _, model := range filtered {
		got = append(got, model.ID)
	}
	require.Equal(t, want, got)
	key := Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.Equal(t, models, filterRetiredOpenAIModelIDs([]Account{account, key}, models), "mixed pools retain models supported by an API-key upstream")
	account.Credentials["model_mapping"].(map[string]any)["public-*"] = "gpt-5.5"
	require.Equal(t, []string{"public-retired-live", "gpt-5.5"}, filterRetiredOpenAIModelIDs([]Account{account}, models), "mapping snapshots refresh between filtering calls")
}

func TestUpgradeRetirementPassthroughChecksFinalPayloadBeforeCredentials(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5.5": "gpt-5.6"}},
		Extra:       map[string]any{"openai_passthrough": true}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	// Normal mappings do not rewrite passthrough payloads. An account without
	// credentials/upstream must reject this retired actual target before I/O.
	svc := &OpenAIGatewayService{}
	result, err := svc.forwardOpenAIPassthrough(context.Background(), c, account,
		[]byte(`{"model":"gpt-5.5","input":"hello"}`), "gpt-5.5", nil, false, time.Now())
	require.Nil(t, result)
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, http.StatusBadRequest, failure.StatusCode)
	require.True(t, failure.SkipPoolSoftCooldown)
	require.True(t, failure.SkipSchedulePenalty)
	require.True(t, failure.SkipStickySessionEviction)
}

func TestUpgradeAnthropicBuildersRejectForeignOAuthBeforeRequest(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	svc := &GatewayService{}
	for _, platform := range []string{PlatformOpenAI, PlatformGemini, PlatformAntigravity} {
		for _, kind := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
			a := &Account{Platform: platform, Type: kind}
			builders := []func() (*http.Request, error){
				func() (*http.Request, error) {
					return svc.buildUpstreamRequest(context.Background(), c, a, nil, "private-token", "oauth", "claude-sonnet-4-6", false, false)
				},
				func() (*http.Request, error) {
					return svc.buildCountTokensRequest(context.Background(), c, a, nil, "private-token", "oauth", "claude-sonnet-4-6", false)
				},
				func() (*http.Request, error) {
					return svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(context.Background(), c, a, nil, "private-token")
				},
				func() (*http.Request, error) {
					return svc.buildCountTokensRequestAnthropicAPIKeyPassthrough(context.Background(), c, a, nil, "private-token")
				},
			}
			for _, build := range builders {
				req, err := build()
				require.Nil(t, req)
				var failure *UpstreamFailoverError
				require.ErrorAs(t, err, &failure)
				require.True(t, failure.SkipSchedulePenalty)
				require.True(t, failure.SkipStickySessionEviction)
				require.True(t, failure.SkipPoolSoftCooldown)
			}
		}
	}
	a := &Account{Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_protocol": "anthropic", "base_url": "https://messages.example.com"}}
	require.True(t, anthropicMessagesCredentialAllowed(a))
	require.Equal(t, "https://messages.example.com", getAnthropicGatewayBaseURL(a))
}

func TestUpgradeConfiguredTokenFloorPreservesLargerAndOmittedLimits(t *testing.T) {
	for _, tc := range []struct{ floor, limit, want int }{{16, 1, 16}, {256, 32, 256}, {256, 512, 512}, {0, 1, 128}, {-1, 1, 128}, {256, 0, 0}} {
		cfg := &config.Config{}
		cfg.Gateway.MinOutputTokens = tc.floor
		req := &apicompat.ChatCompletionsRequest{Model: "gpt-5.6"}
		if tc.limit > 0 {
			req.MaxTokens = &tc.limit
		}
		out, err := chatCompletionsToResponses(cfg, req)
		require.NoError(t, err)
		anth, err := anthropicToResponses(cfg, &apicompat.AnthropicRequest{Model: "gpt-5.6", MaxTokens: tc.limit})
		require.NoError(t, err)
		if tc.want == 0 {
			require.Nil(t, out.MaxOutputTokens)
			require.Nil(t, anth.MaxOutputTokens)
		} else {
			require.Equal(t, tc.want, *out.MaxOutputTokens)
			require.Equal(t, tc.want, *anth.MaxOutputTokens)
		}
	}
}

func TestUpgradeArgonPromotionAndCustomPricesRemainIsolated(t *testing.T) {
	billing := NewBillingService(nil, nil)
	eligible, ineligible := true, false
	tokens := UsageTokens{InputTokens: 1000000, OutputTokens: 1000000, CacheReadTokens: 1000000}
	for _, tc := range []struct {
		model    string
		at       time.Time
		eligible *bool
		want     float64
	}{
		{"gemini-4-argon-high", geminiArgonLaunchOfferEnd.Add(-time.Nanosecond), &eligible, 12.1},
		{"gemini-4-argon-high", geminiArgonLaunchOfferEnd, &eligible, 24.2},
		{"gemini-4-argon", geminiArgonLaunchOfferEnd.Add(-time.Hour), &ineligible, 24.2},
		{"gemini-4-argon-cyber-permissive", geminiArgonLaunchOfferEnd.Add(-time.Hour), &eligible, 24.2},
	} {
		cost, err := billing.CalculateCostUnified(CostInput{Ctx: context.Background(), Model: tc.model, Tokens: tokens, RateMultiplier: 1, PricingAt: tc.at, GeminiLaunchOfferEligible: tc.eligible})
		require.NoError(t, err)
		require.InDelta(t, tc.want, cost.TotalCost, 1e-10)
	}
	price := &ModelPricing{InputPricePerToken: 9, OutputPricePerToken: 8, CacheReadPricePerToken: 7, PricingOverrideInput: true, PricingOverrideOutput: true, PricingOverrideCacheRead: true}
	copy := applyGemini36OfficialPricing("gemini-4-argon", price, geminiArgonLaunchOfferEnd)
	require.Equal(t, *price, *copy)
	require.NotSame(t, price, copy)
}

func TestUpgradeKeyedCatalogUsesExactGenerationIDs(t *testing.T) {
	models, err := extractUpstreamModelIDs([]byte(`{"models":{"gemini-4-argon-high":{"id":"wrong","displayName":"Argon High","supportsThinking":true},"claude-sonnet-5-5":null}}`))
	require.NoError(t, err)
	require.Equal(t, []string{"claude-sonnet-5-5", "gemini-4-argon-high"}, models)
	_, err = extractUpstreamModelIDs([]byte(`{"models":"invalid"}`))
	require.Error(t, err)
}

func TestUpgradeZhipuCreditWindowsUseUnitsEvenWhenWeeklyResetsEarlier(t *testing.T) {
	tiers := parseZhipuTokenTiers(gjson.Parse(`{"limits":[{"type":"CREDIT_LIMIT","unit":6,"percentage":77,"nextResetTime":1800000000000},{"type":"CREDIT_LIMIT","unit":3,"percentage":22,"nextResetTime":1800010000000}]}`))
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.Equal(t, 22.0, tiers[0].UsedPercent)
	require.Equal(t, "weekly", tiers[1].Window)
	require.Equal(t, 77.0, tiers[1].UsedPercent)
}

func TestUpgradeAntigravityImageTestAndThoughtSignatures(t *testing.T) {
	svc := &AntigravityGatewayService{}
	require.True(t, shouldUseAntigravityGeminiTestRequest("public-claude-alias", "gemini-3.1-flash-image"))
	body, err := svc.buildGeminiTestRequestWithPrompt("project", "gemini-3.1-flash-image", "draw a blue circle")
	require.NoError(t, err)
	require.Equal(t, "draw a blue circle", gjson.GetBytes(body, "request.contents.0.parts.0.text").String())
	require.Equal(t, "IMAGE", gjson.GetBytes(body, "request.generationConfig.responseModalities.1").String())
	body, err = svc.buildGeminiTestRequestWithPrompt("project", "gemini-4-argon-high", "")
	require.NoError(t, err)
	require.Equal(t, "VALIDATED", gjson.GetBytes(body, "request.toolConfig.functionCallingConfig.mode").String())
	text, images := extractContentFromSSEResponse([]byte("data: " + `{"response":{"candidates":[{"content":{"parts":[{"text":"generated"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}}]}}` + "\n\n"))
	require.Equal(t, "generated", text)
	require.Len(t, images, 1)
	require.Equal(t, "data:image/png;base64,aGVsbG8=", images[0].ImageURL)
	response := mergeCollectedPartsToResponse(map[string]any{}, []map[string]any{{"text": "one"}, {"text": "two", "thoughtSignature": "sig-valid"}})
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "sig-valid")
	require.Contains(t, string(encoded), "onetwo")
}
