package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGrokFreeCacheRouteV160NativeResponsesIntegration(t *testing.T) {
	setGinTestMode()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_free_cache","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID: 160, Platform: PlatformGrok, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "grok-token", "subscription_tier": "free"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 1601})
	body := []byte(`{"model":"grok-4.5","input":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{}}]}`)

	result, err := svc.forwardGrokResponses(context.Background(), c, account, body, "grok-4.5", false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotEmpty(t, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
	require.Len(t, gjson.GetBytes(upstream.lastBody, "tools").Array(), 3)
}

func TestGrokMessagesInvalidEncryptedContentRetriesAtMostOnce(t *testing.T) {
	setGinTestMode()
	errorBody := `{"error":{"code":"invalid_encrypted_content","message":"encrypted_content could not decrypt"}}`
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(errorBody)),
		},
		{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(errorBody)),
		},
	}}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled: false, AllowInsecureHTTP: true,
		}}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID: 163, Platform: PlatformGrok, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "grok-token", "base_url": "http://upstream.example/v1"},
	}
	body := []byte(`{"model":"grok-4.5","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"prior reasoning","signature":"xai-provider-signature"},{"type":"text","text":"prior answer"}]},{"role":"user","content":"continue"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	require.Error(t, err)
	require.Len(t, upstream.bodies, 2, "encrypted-content recovery must issue at most one retry")
	require.True(t, gjson.GetBytes(upstream.bodies[0], "input.0.encrypted_content").Exists())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "input.0.encrypted_content").Exists())
}

func TestGrokFreeCacheRouteV160WSHTTPBridgeIntegration(t *testing.T) {
	setGinTestMode()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ws_free\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
		)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, toolCorrector: NewCodexToolCorrector()}
	account := &Account{
		ID: 161, Platform: PlatformGrok, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "grok-token", "subscription_tier": "free"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	payload := []byte(`{"type":"response.create","model":"grok-4.5","input":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{}}]}`)

	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
		context.Background(), c, account, "grok-token", payload, len(payload), "grok-4.5", "", "", "", "tenant-isolated", 1,
		func([]byte) error { return nil },
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "tenant-isolated", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
	require.Len(t, gjson.GetBytes(upstream.lastBody, "tools").Array(), 3)
}

func TestGrokFreeCacheRouteV160ConvertsAndDeduplicatesSearchFunctions(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"subscription_tier": "free",
		},
	}
	body := []byte(`{"model":"grok-build","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"function","name":"web_search","parameters":{}},{"type":"function","name":"x_search","parameters":{}},{"type":"function","name":"web_search","parameters":{}}]}`)

	patched, err := applyGrokFreeMessagesFunctionToolCacheRoute(body, body, account, "tenant-isolated")
	require.NoError(t, err)
	tools := gjson.GetBytes(patched, "tools").Array()
	require.Len(t, tools, 3)
	require.Equal(t, "lookup", tools[0].Get("name").String())
	require.Equal(t, "web_search", tools[1].Get("type").String())
	require.Equal(t, "x_search", tools[2].Get("type").String())
	require.False(t, tools[1].Get("name").Exists())
}

func TestGrokFreeCacheRouteV160GrokBuildDeduplicatesExistingNativeSearchTools(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"subscription_tier": "free",
		},
	}
	intent := []byte(`{"model":"grok-build-latest","tools":[{"type":"function","name":"lookup","parameters":{}},{"type":"function","name":"web_search","parameters":{}},{"type":"function","name":"x_search","parameters":{}}]}`)
	body := []byte(`{"model":"grok-build-latest","tools":[{"type":"function","name":"lookup","parameters":{}},{"type":"function","name":"web_search","parameters":{}},{"type":"function","name":"x_search","parameters":{}},{"type":"web_search"},{"type":"x_search"}]}`)

	patched, err := applyGrokFreeMessagesFunctionToolCacheRoute(body, intent, account, "tenant-isolated")
	require.NoError(t, err)
	tools := gjson.GetBytes(patched, "tools").Array()
	require.Len(t, tools, 3)
	require.Equal(t, "lookup", tools[0].Get("name").String())
	require.Equal(t, "web_search", tools[1].Get("type").String())
	require.Equal(t, "x_search", tools[2].Get("type").String())
}

func TestGrokFreeCacheRouteV160OrdinaryModelPreservesReservedCustomFunctions(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"subscription_tier": "free",
		},
	}
	body := []byte(`{"model":"grok-4.5","tools":[{"type":"function","name":"web_search","description":"tenant custom search","parameters":{"type":"object","properties":{"query":{"type":"string"}}}},{"type":"function","name":"x_search","parameters":{"type":"object"}}]}`)

	patched, err := applyGrokFreeMessagesFunctionToolCacheRoute(body, body, account, "tenant-isolated")
	require.NoError(t, err)
	require.Equal(t, string(body), string(patched), "ordinary models must preserve reserved custom function schemas and avoid conflicting native tools")
}

func TestGrokFreeCacheRouteV160OrdinaryFunctionStillAddsNativeSearchTools(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"subscription_tier": "free",
		},
	}
	body := []byte(`{"model":"grok-4.5","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)

	patched, err := applyGrokFreeMessagesFunctionToolCacheRoute(body, body, account, "tenant-isolated")
	require.NoError(t, err)
	tools := gjson.GetBytes(patched, "tools").Array()
	require.Len(t, tools, 3)
	require.Equal(t, "lookup", tools[0].Get("name").String())
	require.Equal(t, "web_search", tools[1].Get("type").String())
	require.Equal(t, "x_search", tools[2].Get("type").String())
}

func TestGrokFreeCacheRouteV160StrictNoOps(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"lookup","parameters":{}}]}`)
	for _, account := range []*Account{
		{Platform: PlatformGrok, Type: AccountTypeAPIKey},
		{Platform: PlatformGrok, Type: AccountTypeOAuth},
		{Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{"subscription_tier": "SuperGrok"}},
	} {
		patched, err := applyGrokFreeMessagesFunctionToolCacheRoute(body, body, account, "tenant-isolated")
		require.NoError(t, err)
		require.Equal(t, string(body), string(patched))
	}
	free := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{"subscription_tier": "free"}}
	patched, err := applyGrokFreeMessagesFunctionToolCacheRoute(body, body, free, "")
	require.NoError(t, err)
	require.Equal(t, string(body), string(patched))
}

func TestGrokMediaV160AcceptsAndCanonicalizesURLAliases(t *testing.T) {
	body := []byte(`{
		"model":"grok-imagine-video-1.5",
		"image":{"url":"https://example.com/source.png","image_url":"https://example.com/ignored.png"},
		"reference_images":[{"image_url":"https://example.com/reference.png"}],
		"mask":{"image_url":"https://example.com/mask.png"}
	}`)
	info := ParseGrokMediaRequest("application/json", body)
	require.Equal(t, []string{"https://example.com/source.png", "https://example.com/reference.png"}, info.InputImageURLs)
	require.Equal(t, "https://example.com/mask.png", info.MaskImageURL)
	require.True(t, info.HasInputImage())

	out, err := canonicalizeGrokMediaImageURLFields(body, "image", "reference_images", "mask")
	require.NoError(t, err)
	require.Equal(t, "https://example.com/source.png", gjson.GetBytes(out, "image.url").String())
	require.False(t, gjson.GetBytes(out, "image.image_url").Exists())
	require.Equal(t, "https://example.com/reference.png", gjson.GetBytes(out, "reference_images.0.url").String())
	require.False(t, gjson.GetBytes(out, "reference_images.0.image_url").Exists())
	require.Equal(t, "https://example.com/mask.png", gjson.GetBytes(out, "mask.url").String())

	directMask := ParseGrokMediaRequest("application/json", []byte(`{"mask":"https://example.com/direct-mask.png"}`))
	require.Equal(t, "https://example.com/direct-mask.png", directMask.MaskImageURL)
}

func TestGrokMediaV160PreservesImageToVideoModel(t *testing.T) {
	body := []byte(`{"model":"grok-imagine-video-1.5","image":{"image_url":"data:image/png;base64,aW1n"}}`)
	out, _, err := normalizeGrokMediaForwardBody(GrokMediaEndpointVideosGenerations, body, "application/json")
	require.NoError(t, err)
	require.Equal(t, "grok-imagine-video-1.5", gjson.GetBytes(out, "model").String())
	require.Equal(t, "data:image/png;base64,aW1n", gjson.GetBytes(out, "image.url").String())
}

func TestGrokMediaV160VideoModelFallbackIsGenerationOnly(t *testing.T) {
	require.Equal(t, "grok-imagine-video",
		normalizeGrokMediaModelForEndpoint(GrokMediaEndpointVideosGenerations, "grok-imagine-video-1.5", false))
	require.Equal(t, "grok-imagine-video-1.5",
		normalizeGrokMediaModelForEndpoint(GrokMediaEndpointVideosGenerations, "grok-imagine-video-1.5", true))
	for _, endpoint := range []GrokMediaEndpoint{GrokMediaEndpointVideosEdits, GrokMediaEndpointVideosExtensions} {
		require.Equal(t, "grok-imagine-video-1.5",
			normalizeGrokMediaModelForEndpoint(endpoint, "grok-imagine-video-1.5", false))
	}
}

func TestGrokMediaGenerationEligibilityV160(t *testing.T) {
	forbidden := &xai.BillingSummary{StatusCode: http.StatusOK, WeeklyStatusCode: http.StatusForbidden, MonthlyStatusCode: http.StatusOK}
	tests := []struct {
		name   string
		acct   *Account
		want   bool
		reason string
	}{
		{name: "unobserved oauth", acct: &Account{Platform: PlatformGrok, Type: AccountTypeOAuth}, want: true, reason: "billing_unobserved"},
		{name: "api key", acct: &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey}, want: true, reason: "non_oauth"},
		{name: "custom oauth 403 stays unknown", acct: &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{"base_url": "https://relay.example/v1"}, Extra: map[string]any{GrokBillingSnapshotExtraKey: forbidden}}, want: true, reason: "custom_billing_unobserved"},
		{name: "forbidden", acct: &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, Extra: map[string]any{GrokBillingSnapshotExtraKey: forbidden}}, want: false, reason: "billing_forbidden"},
		{name: "forced allow", acct: &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, Extra: map[string]any{GrokMediaEligibleExtraKey: true, GrokBillingSnapshotExtraKey: forbidden}}, want: true, reason: "override_enabled"},
		{name: "forced deny", acct: &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey, Extra: map[string]any{GrokMediaEligibleExtraKey: false}}, want: false, reason: "override_disabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := tt.acct.GrokMediaGenerationEligibility()
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.reason, reason)
		})
	}
}

func TestGrokMediaEligibilityV160FreshSchedulerRecheckRejectsForbidden(t *testing.T) {
	account := &Account{
		ID: 162, Platform: PlatformGrok, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Extra: map[string]any{GrokBillingSnapshotExtraKey: map[string]any{"weekly_status_code": http.StatusForbidden}},
	}
	repo := stubOpenAIAccountRepo{accounts: []Account{*account}}
	svc := &OpenAIGatewayService{
		accountRepo: repo,
		schedulerSnapshot: NewSchedulerSnapshotService(&openAISnapshotCacheStub{
			accountsByID: map[int64]*Account{account.ID: account},
		}, nil, repo, nil, nil, nil),
	}

	got := svc.recheckSelectedOpenAIAccountFromDB(
		context.Background(), &Account{ID: account.ID}, "", false,
		OpenAIEndpointCapabilityGrokMediaGeneration, "", PlatformGrok,
	)
	require.Nil(t, got)
}

func TestNoAvailableOpenAISelectionErrorV160PreservesSentinel(t *testing.T) {
	err := noAvailableOpenAISelectionError("grok-imagine-1.0", false)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Contains(t, err.Error(), "grok-imagine-1.0")
	require.True(t, errors.Is(noAvailableOpenAISelectionError("", false), ErrNoAvailableAccounts))
	require.ErrorIs(t, noAvailableOpenAISelectionError("", true), ErrNoAvailableCompactAccounts)
}

func TestExplicitImageGenerationIntentV160(t *testing.T) {
	passive := []byte(`{"model":"gpt-5.6-sol","tools":[{"type":"namespace","name":"image_gen"}],"input":[{"type":"additional_tools","tools":[{"type":"image_generation"}]}],"tool_choice":"auto"}`)
	require.False(t, IsExplicitImageGenerationIntent("/v1/responses", "gpt-5.6-sol", passive))
	require.True(t, IsImageGenerationIntent("/v1/responses", "gpt-5.6-sol", passive))

	for _, body := range [][]byte{
		[]byte(`{"model":"gpt-5.6-sol","tools":[{"type":"image_generation"}]}`),
		[]byte(`{"model":"gpt-5.6-sol","tool_choice":{"type":"namespace","name":"image_gen"}}`),
		[]byte(`{"model":"gpt-5.6-sol","input":[{"type":"function_call","name":"image_gen.imagegen"}]}`),
	} {
		require.True(t, IsExplicitImageGenerationIntent("/v1/responses", "gpt-5.6-sol", body))
	}
}

func TestExplicitImageGenerationIntentMapV160IgnoresPassiveCatalog(t *testing.T) {
	request := map[string]any{
		"model":       "gpt-5.6-sol",
		"tools":       []any{map[string]any{"type": "namespace", "name": "image_gen"}},
		"input":       []any{map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "image_generation"}}}},
		"tool_choice": "auto",
	}
	require.False(t, IsExplicitImageGenerationIntentMap("/v1/responses", "gpt-5.6-sol", request))

	for _, toolChoice := range []any{
		map[string]any{"type": "function", "name": "image_gen.imagegen"},
		map[string]any{"type": "function", "namespace": "image_gen", "name": "imagegen"},
		map[string]any{"type": "function", "function": map[string]any{"name": "image_gen.imagegen"}},
	} {
		require.True(t, IsExplicitImageGenerationIntentMap("/v1/responses", "gpt-5.6-sol", map[string]any{
			"model":       "gpt-5.6-sol",
			"tool_choice": toolChoice,
		}))
	}
}

func TestAlphaSearchV160AllowsSyntacticallyValidCustomAPIKeyUpstream(t *testing.T) {
	require.True(t, IsOpenAIAlphaSearchAccountEligible(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://relay.example/v1",
		},
	}))
	require.False(t, IsOpenAIAlphaSearchAccountEligible(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://user:pass@relay.example/v1",
		},
	}))
}

func TestGrokMediaEligibilityExtraV160TriState(t *testing.T) {
	normalized, err := normalizeGrokMediaEligibilityExtra(PlatformGrok, map[string]any{GrokMediaEligibleExtraKey: nil})
	require.NoError(t, err)
	require.NotContains(t, normalized, GrokMediaEligibleExtraKey)

	_, err = normalizeGrokMediaEligibilityExtra(PlatformGrok, map[string]any{GrokMediaEligibleExtraKey: "false"})
	require.Error(t, err)

	account := &Account{Platform: PlatformGrok, Extra: map[string]any{GrokMediaEligibleExtraKey: true}}
	input := &UpdateAccountInput{Extra: map[string]any{
		GrokMediaEligibleExtraKey:   false,
		GrokBillingSnapshotExtraKey: map[string]any{"status_code": http.StatusForbidden},
	}}
	updated, err := normalizeGrokMediaEligibilityUpdateExtra(account, input, input.Extra)
	require.NoError(t, err)
	require.Equal(t, false, updated[GrokMediaEligibleExtraKey])
	require.NotContains(t, updated, GrokBillingSnapshotExtraKey)
}

func TestGrokMediaEligibilityOnlyPatchPreservesRuntimeAndCustomExtra(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Extra: map[string]any{
			"custom_setting":            "keep",
			GrokQuotaSnapshotExtraKey:   map[string]any{"status_code": http.StatusOK},
			GrokBillingSnapshotExtraKey: map[string]any{"status_code": http.StatusOK},
			GrokMediaEligibleExtraKey:   true,
		},
	}
	input := &UpdateAccountInput{Extra: map[string]any{GrokMediaEligibleExtraKey: false}}
	updated, err := normalizeGrokMediaEligibilityUpdateExtra(account, input, input.Extra)
	require.NoError(t, err)
	require.Equal(t, false, updated[GrokMediaEligibleExtraKey])
	require.Equal(t, "keep", updated["custom_setting"])
	require.Contains(t, updated, GrokQuotaSnapshotExtraKey)
	require.Contains(t, updated, GrokBillingSnapshotExtraKey)

	input.Extra = map[string]any{GrokMediaEligibleExtraKey: nil}
	updated, err = normalizeGrokMediaEligibilityUpdateExtra(account, input, input.Extra)
	require.NoError(t, err)
	require.NotContains(t, updated, GrokMediaEligibleExtraKey)
	require.Contains(t, updated, GrokQuotaSnapshotExtraKey)
}

func TestGrokBillingStatusV160DoesNotRecoverOnAmbiguousFailure(t *testing.T) {
	require.Equal(t, http.StatusForbidden, mergeGrokBillingWindowStatus(http.StatusForbidden, 0, false))
	require.Equal(t, http.StatusForbidden, mergeGrokBillingWindowStatus(http.StatusForbidden, http.StatusTooManyRequests, false))
	require.Equal(t, http.StatusOK, mergeGrokBillingWindowStatus(http.StatusForbidden, http.StatusOK, true))

	previous := &xai.BillingSummary{StatusCode: http.StatusForbidden}
	require.Equal(t, http.StatusForbidden, mergeGrokBillingOverallStatus(previous, 0, http.StatusOK, false, true))
	require.Equal(t, http.StatusOK, mergeGrokBillingOverallStatus(previous, http.StatusOK, http.StatusOK, true, true))

	windowed := &xai.BillingSummary{
		StatusCode:        http.StatusForbidden,
		WeeklyStatusCode:  http.StatusForbidden,
		MonthlyStatusCode: http.StatusOK,
	}
	require.Equal(t, http.StatusOK, mergeGrokBillingOverallStatus(windowed, http.StatusOK, 0, true, false),
		"a successful refresh of the forbidden window must recover routing")
}
