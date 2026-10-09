package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestV0215GrokEmptyCompletedOnlyFailsOverWithoutUsageOrOutput(t *testing.T) {
	for _, tc := range []struct {
		name, preamble, terminal string
		failover                 bool
	}{
		{"empty", "", `{"output":[]}`, true},
		{"usage", "", `{"output":[],"usage":{"input_tokens":3,"output_tokens":4}}`, false},
		{"zero usage", "", `{"output":[],"usage":{"input_tokens":0,"output_tokens":0}}`, false},
		{"output", "", `{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`, false},
		{"delta", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n", `{"output":[]}`, false},
		{"refusal", "", `{"output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`, false},
		{"nested error", "", `{"output":[],"error":{"message":"failed"}}`, false},
		{"error event", "event: error\ndata: {\"message\":\"failed\"}\n\n", `{"output":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"grok-test\"}}\n\n" + tc.preamble + `data: {"type":"response.completed","response":` + tc.terminal + "}\n\n"))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, codexSnapshotThrottle: newAccountWriteThrottle(time.Minute)}
			account := &Account{ID: 78, Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "test-token"}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"grok","input":"hi","stream":true}`))
			if tc.failover {
				var failure *UpstreamFailoverError
				require.True(t, errors.As(err, &failure), "%v", err)
				require.Empty(t, recorder.Body.String())
			} else if tc.name == "nested error" || tc.name == "error event" {
				require.Error(t, err)
				require.NotContains(t, err.Error(), openAISilentRefusalErrorCode)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Contains(t, recorder.Body.String(), "response.completed")
				if tc.name == "usage" {
					require.Equal(t, 3, result.Usage.InputTokens)
					require.Equal(t, 4, result.Usage.OutputTokens)
				}
			}
		})
	}
}

func TestV0215CompositeCatalogUsesInferencePrecedence(t *testing.T) {
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: 7, PublicModel: "public", MatchType: "exact", Endpoint: "any", TargetPlatform: PlatformOpenAI, Enabled: true},
		{ID: 2, GroupID: 7, PublicModel: "public", MatchType: "exact", Endpoint: "responses", TargetPlatform: PlatformTypeSafe, Enabled: true},
		{ID: 3, GroupID: 7, PublicModel: "image", MatchType: "exact", Endpoint: "images", TargetPlatform: PlatformGrok, Enabled: true},
		{ID: 4, GroupID: 7, PublicModel: "visible", MatchType: "exact", TargetPlatform: PlatformGrok, Enabled: true},
		{ID: 5, GroupID: 7, PublicModel: "disabled", MatchType: "exact", TargetPlatform: PlatformGrok},
		{ID: 6, GroupID: 7, PublicModel: "prefix-", MatchType: "prefix", TargetPlatform: PlatformOpenAI, Enabled: true},
	}
	resolver := &CompositeRouteResolver{repo: &compositeRouteRepoStub{routes: routes}}
	models, err := resolver.CatalogModels(context.Background(), 7, []string{"mapped"}, CompositeRouteEndpointResponses, false)
	require.NoError(t, err)
	require.Equal(t, []string{"mapped", "visible"}, models)
	models, err = resolver.CatalogModels(context.Background(), 7, nil, "", true)
	require.NoError(t, err)
	require.Equal(t, []string{"image", "public", "visible"}, models)
}

func TestV0215CodexGroupProjectionAndCapabilityIntersection(t *testing.T) {
	body := []byte(`{"models":[{"slug":"allowed","service_tiers":["fast"],"supports_search_tool":true,"use_responses_lite":true,"future":42},{"slug":"private"}]}`)
	group := &Group{ModelsListConfig: GroupModelsListConfig{Enabled: true, Models: []string{"allow*"}}}
	filtered, err := filterCodexManifestForGroup(body, group)
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(filtered, "models").Array(), 1)
	merged, err := mergePinnedCodexManifestBodies([][]byte{filtered, []byte(`{"models":[{"slug":"allowed","service_tiers":[],"supports_search_tool":false}]}`)})
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(merged, "models.0.supports_search_tool").Bool())
	require.False(t, gjson.GetBytes(merged, "models.0.use_responses_lite").Bool())
	require.Equal(t, "[]", gjson.GetBytes(merged, "models.0.service_tiers").Raw)
	require.Equal(t, int64(42), gjson.GetBytes(merged, "models.0.future").Int())
	require.NotEqual(t, codexModelsManifestBodyETag(body), codexModelsManifestBodyETag(filtered))
	empty, err := mergePinnedCodexManifestBodies([][]byte{[]byte(`{"models":[]}`)})
	require.NoError(t, err)
	require.Equal(t, "[]", gjson.GetBytes(empty, "models").Raw)
}

func TestV0215ZenProtocolPreservesGoAndCustomRules(t *testing.T) {
	account := &Account{Platform: PlatformOpenCodeGo, Credentials: map[string]any{"account_mode": AccountModeZen}}
	require.Equal(t, APIProtocolChatCompletions, account.ResolveOpenCodeGoUpstreamProtocol("qwen3.8-max"))
	require.Error(t, validateStandardOpenCodeZenModel(account, "gemini-3"))
	require.Error(t, validateStandardOpenCodeZenModel(account, "jev-latest"))
	account.Credentials["account_mode"] = AccountModeGo
	require.Equal(t, APIProtocolAnthropic, account.ResolveOpenCodeGoUpstreamProtocol("qwen3.8-max"))
	require.NoError(t, validateStandardOpenCodeZenModel(account, "gemini-3"))
	account.Credentials["account_mode"] = AccountModeZen
	account.Credentials[openCodeGoProtocolRulesKey] = []any{map[string]any{"pattern": "*", "protocol": APIProtocolResponses}}
	require.Equal(t, APIProtocolResponses, account.ResolveOpenCodeGoUpstreamProtocol("qwen3.8-max"))
	require.NoError(t, validateStandardOpenCodeZenModel(account, "gemini-3"))
	account.Credentials[openCodeGoProtocolRulesKey] = []any{map[string]any{"pattern": "qwen*", "protocol": APIProtocolAnthropic}}
	require.Error(t, validateStandardOpenCodeZenModel(account, "gemini-3"))
	account.Credentials[openCodeGoProtocolRulesKey] = "invalid"
	require.Error(t, validateStandardOpenCodeZenModel(account, "gemini-3"))
	account.Credentials["base_url"] = "https://custom.example/v1"
	require.NoError(t, validateStandardOpenCodeZenModel(account, "gemini-3"))
}

func TestV0215EncryptedReasoningRejectionIsNarrow(t *testing.T) {
	message := "The encrypted content could not be verified; it could not be decrypted or parsed"
	require.True(t, isRejectedOpenAIEncryptedReasoning("invalid_encrypted_content", ""))
	require.True(t, isRejectedOpenAIEncryptedReasoning("thinking_signature_invalid", message))
	for _, code := range []string{"", "invalid_request_error", "execution_unknown"} {
		require.False(t, isRejectedOpenAIEncryptedReasoning(code, message))
	}
	for _, message := range []string{"Invalid signature", "The encrypted content could not be verified", "could not be decrypted or parsed"} {
		require.False(t, isRejectedOpenAIEncryptedReasoning("thinking_signature_invalid", message))
	}
}

func TestV0215MimicBetaHonorsExplicitRequestAndDrops(t *testing.T) {
	svc := &GatewayService{}
	headers := http.Header{"Anthropic-Beta": {claude.BetaInlineTools + "," + claude.BetaMidConversationToolChanges + ",unknown-beta"}}
	beta, set := svc.computeFinalAnthropicBeta("oauth", true, "claude-haiku", headers, nil, map[string]struct{}{claude.BetaInlineTools: {}})
	require.True(t, set)
	require.True(t, containsBetaToken(beta, claude.BetaMidConversationToolChanges))
	require.False(t, containsBetaToken(beta, claude.BetaInlineTools))
	require.False(t, containsBetaToken(beta, "unknown-beta"))
	base, _ := svc.computeFinalAnthropicBeta("oauth", true, "claude-haiku", nil, nil, nil)
	require.False(t, containsBetaToken(base, claude.BetaMidConversationToolChanges))
}

func TestV0215MimicCacheOrderPreservesLongTTL(t *testing.T) {
	body := []byte(`{"tools":[{"cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[{"role":"user","content":[{"type":"text","text":"long","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"tail","cache_control":{"type":"ephemeral","ttl":"5m"}}]}],"prompt_cache_key":"keep"}`)
	got := normalizeMimicCacheControlTTLOrder(body)
	require.Equal(t, "1h", gjson.GetBytes(got, "tools.0.cache_control.ttl").String())
	require.Equal(t, "1h", gjson.GetBytes(got, "system.0.cache_control.ttl").String())
	require.Equal(t, "5m", gjson.GetBytes(got, "messages.0.content.1.cache_control.ttl").String())
	require.Equal(t, "keep", gjson.GetBytes(got, "prompt_cache_key").String())
	require.Equal(t, got, normalizeMimicCacheControlTTLOrder(got))
	valid := []byte(`{ "tools": [{"cache_control":{"type":"ephemeral","ttl":"1h"}}], "messages":[] }`)
	require.Equal(t, valid, normalizeMimicCacheControlTTLOrder(valid))
}

func TestV0215NativeMessagesMimicCacheOrder(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		t.Run(map[bool]string{false: "apikey", true: "oauth mimic"}[oauth], func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"tools":[{"name":"test","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)),
			}}
			account := newAnthropicAPIKeyAccountForTest()
			account.Extra = nil
			if oauth {
				account.Type = AccountTypeOAuth
				account.Credentials["access_token"] = "test-oauth-token"
			}
			svc := &GatewayService{cfg: &config.Config{}, httpUpstream: upstream, deferredService: &DeferredService{}, rateLimitService: &RateLimitService{}}
			_, err := svc.Forward(context.Background(), c, account, &ParsedRequest{Body: body, Model: "claude-sonnet-4-5"})
			require.NoError(t, err)
			wantTTL := "5m"
			if oauth {
				wantTTL = "1h"
			}
			require.Equal(t, wantTTL, gjson.GetBytes(upstream.lastBody, "tools.0.cache_control.ttl").String())
			require.Equal(t, "1h", gjson.GetBytes(upstream.lastBody, "messages.0.content.0.cache_control.ttl").String())
		})
	}
}

func TestV0215AnthropicChatBufferRejectsOutOfRangeDeltas(t *testing.T) {
	for _, index := range []int{-1, 2} {
		responseWriter := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(responseWriter)
		resp := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_safe","role":"assistant","content":[],"usage":{"input_tokens":5}}}`,
			``, `event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"kept"}}`,
			``, `event: content_block_delta`,
			`data: {"type":"content_block_delta","index":` + strconv.Itoa(index) + `,"delta":{"type":"text_delta","text":"invalid"}}`,
			``, `event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			``, `event: message_stop`, `data: {"type":"message_stop"}`, ``,
		}, "\n")))}
		result, err := (&GatewayService{}).handleCCBufferedFromAnthropic(resp, c, "gpt-5", "claude-sonnet-4-5", nil, time.Now())
		require.NoError(t, err)
		require.Equal(t, 5, result.Usage.InputTokens)
		require.Equal(t, 2, result.Usage.OutputTokens)
		require.Equal(t, "kept", gjson.GetBytes(responseWriter.Body.Bytes(), "choices.0.message.content").String())
	}
}

func TestV0215CodexProjectionDoesNotMutateSharedCatalog(t *testing.T) {
	body := []byte(`{ "unknown": {"keep":123}, "models": [{"slug":"upstream","supports_search_tool":true,"service_tiers":null,"custom":{"keep":true}},{"slug":"private"}] }`)
	original := bytes.Clone(body)
	account := &Account{Platform: PlatformOpenAI, Credentials: map[string]any{"model_mapping": map[string]any{"public": "upstream"}}}
	got, err := projectCodexManifestForAccount(body, account)
	require.NoError(t, err)
	require.Equal(t, original, body)
	require.Equal(t, "public", gjson.GetBytes(got, "models.0.slug").String())
	require.Equal(t, `[]`, gjson.GetBytes(got, "models.0.service_tiers").Raw)
	require.Equal(t, 1, len(gjson.GetBytes(got, "models").Array()))
	require.True(t, gjson.GetBytes(got, "models.0.custom.keep").Bool())
	require.Equal(t, int64(123), gjson.GetBytes(got, "unknown.keep").Int())
	account.Credentials["model_mapping"] = map[string]any{"upstream": "upstream", "private": "private"}
	valid := []byte(`{ "models": [ {"slug":"upstream","service_tiers":[]}, {"slug":"private"} ] }`)
	unchanged, err := projectCodexManifestForAccount(valid, account)
	require.NoError(t, err)
	require.Equal(t, valid, unchanged)
}

func TestV0215CodexMappingChangesImmediatelyInvalidateOutputETag(t *testing.T) {
	requests := 0
	upstream := &codexModelsMemoryUpstream{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"ETag": {`"upstream"`}}, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"gpt-source","service_tiers":[{"id":"priority","name":"Fast","description":"fast"}]}]}`))}, nil
	}}
	svc := newCodexModelsAPIKeyTestService(upstream)
	account := newCodexModelsAPIKeyTestAccount("https://upstream.example/v1")
	account.Credentials["model_mapping"] = map[string]any{"public-one": "gpt-source"}
	first, err := svc.FetchCodexModelsManifest(context.Background(), account, "0.153.0", "")
	require.NoError(t, err)
	require.Equal(t, "public-one", gjson.GetBytes(first.Body, "models.0.slug").String())
	account.Credentials["model_mapping"] = map[string]any{"public-two": "gpt-source"}
	second, err := svc.FetchCodexModelsManifest(context.Background(), account, "0.153.0", first.ETag)
	require.NoError(t, err)
	require.False(t, second.NotModified)
	require.NotEqual(t, first.ETag, second.ETag)
	require.Equal(t, "public-two", gjson.GetBytes(second.Body, "models.0.slug").String())
	require.Equal(t, 1, requests)
	require.Equal(t, "public-one", gjson.GetBytes(first.Body, "models.0.slug").String())
}

func TestV0215GrokSmallChatBridgeEmptyCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		failover       bool
	}{
		{"empty", `{"output":[]}`, true},
		{"usage", `{"output":[],"usage":{"input_tokens":1,"output_tokens":0}}`, false},
		{"refusal", `{"output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":` + tc.response + "}\n\n"))}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 99, Platform: PlatformGrok, Type: AccountTypeOAuth}
			_, err := svc.handleChatStreamingResponse(context.Background(), resp, c, account, "grok", "grok", "grok", time.Now(), 10)
			if tc.failover {
				var failure *UpstreamFailoverError
				require.True(t, errors.As(err, &failure))
				require.Empty(t, recorder.Body.String())
			} else {
				require.NoError(t, err)
				require.NotEmpty(t, recorder.Body.String())
			}
		})
	}
}

func TestV0215NamespaceKeepsUntouchedFormattingAndIntegers(t *testing.T) {
	body := []byte(`{ "input": [ {"type":"function_call","namespace":"old","arguments":"{}","n":9007199254740993}, {"type":"message","nested":{"namespace":"keep"}} ], "unknown": 1 }`)
	got, err := stripOpenAIResponsesInputNamespaces(body)
	require.NoError(t, err)
	require.True(t, json.Valid(got))
	require.Contains(t, string(got), `9007199254740993`)
	require.Contains(t, string(got), ` {"type":"message","nested":{"namespace":"keep"}} ], "unknown": 1 }`)
	require.False(t, gjson.GetBytes(got, "input.0.namespace").Exists())
	unchanged, err := stripOpenAIResponsesInputNamespaces(body, true)
	require.NoError(t, err)
	require.Equal(t, body, unchanged)
}

func BenchmarkV0215NamespaceNoChange(b *testing.B) {
	body := []byte(`{"input":[{"type":"function_call","namespace":"keep","arguments":"{}"}],"other":"namespace"}`)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = stripOpenAIResponsesInputNamespaces(body, true)
	}
}

func TestV0215SilentDetectorKeepsRefusalsAndProviderErrors(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.refusal.delta","delta":"no"}`,
		`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}}`,
		`{"type":"response.completed","response":{"error":{"message":"failed"}}}`,
		`{"choices":[{"delta":{"refusal":"no"}}]}`,
		`{"type":"response.output_item.done","item":{"type":"web_search_call"}}`,
	} {
		detector := newOpenAIChatSilentRefusalDetector(openAISilentRefusalMinRequestBodyBytes)
		detector.ObservePayload([]byte(payload))
		detector.ObservePayload([]byte(`{"type":"response.completed","response":{"output":[]}}`))
		require.False(t, detector.IsSilentRefusal(), payload)
		require.True(t, detector.ShouldReleaseClientOutput(), payload)
	}
}

func TestV0215AccountTestRedactsSecretsBeforeTruncation(t *testing.T) {
	for _, message := range []string{
		`{"error":"missing response.completed","authorization":"Bearer secret-auth","api_key":"secret-key","access_token":"secret-access","cookie":"secret-cookie"}`,
		"missing response.completed; Authorization: Bearer secret-auth; api_key=secret-key; access_token=secret-access",
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
		err := (&AccountTestService{}).sendErrorAndEnd(c, message)
		require.ErrorContains(t, err, "response.completed")
		for _, secret := range []string{"secret-auth", "secret-key", "secret-access", "secret-cookie"} {
			require.NotContains(t, recorder.Body.String(), secret)
			require.NotContains(t, err.Error(), secret)
		}
	}
}

// Keep the previous implementation here to compare allocation costs on identical fixtures.
func v0214NamespaceBenchmarkBaseline(body []byte, keep bool) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"namespace"`)) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	items := make([][]byte, 0)
	changed := false
	var stripErr error
	input.ForEach(func(_, item gjson.Result) bool {
		itemBody := []byte(item.Raw)
		if item.IsObject() && item.Get("namespace").Exists() && (!keep || !openAIResponsesToolCallItemTypes[strings.ToLower(strings.TrimSpace(item.Get("type").String()))]) {
			itemBody, stripErr = sjson.DeleteBytes(itemBody, "namespace")
			if stripErr != nil {
				return false
			}
			changed = true
		}
		items = append(items, itemBody)
		return true
	})
	if stripErr != nil || !changed {
		return body, stripErr
	}
	rebuilt := make([]byte, 0, len(input.Raw))
	rebuilt = append(rebuilt, '[')
	for index, item := range items {
		if index > 0 {
			rebuilt = append(rebuilt, ',')
		}
		rebuilt = append(rebuilt, item...)
	}
	rebuilt = append(rebuilt, ']')
	return sjson.SetRawBytes(body, "input", rebuilt)
}

func BenchmarkV0215NamespaceComparison(b *testing.B) {
	for _, fixture := range []struct {
		name, item string
		keep       bool
	}{
		{"no_namespace", `{"type":"message","content":"hello"}`, false},
		{"nested_only", `{"type":"message","content":"hello","nested":{"namespace":"keep"}}`, false},
		{"kept_calls", `{"type":"function_call","namespace":"keep","arguments":"{}"}`, true},
		{"changed", `{"type":"function_call","namespace":"remove","arguments":"{}"}`, false},
	} {
		body := []byte(`{"input":[` + strings.Repeat(fixture.item+",", 511) + fixture.item + `]}`)
		for _, baseline := range []bool{true, false} {
			name := fixture.name + "/new"
			if baseline {
				name = fixture.name + "/baseline"
			}
			b.Run(name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if baseline {
						_, _ = v0214NamespaceBenchmarkBaseline(body, fixture.keep)
					} else {
						_, _ = stripOpenAIResponsesInputNamespaces(body, fixture.keep)
					}
				}
			})
		}
	}
}

func TestV0215UsageRetryAfterIsBounded(t *testing.T) {
	now := time.Unix(100000, 0).UTC()
	for raw, seconds := range map[string]int{"0": 0, "60": 60, "999999999999999999999999999": 86400, now.Add(-time.Hour).Format(http.TimeFormat): 0, now.Add(48 * time.Hour).Format(http.TimeFormat): 86400} {
		at, ok := openCodeUsageRetryAfter(raw, now)
		require.True(t, ok, raw)
		require.Equal(t, time.Duration(seconds)*time.Second, at.Sub(now), raw)
	}
	for _, raw := range []string{"", "-1", "invalid", "0.5"} {
		_, ok := openCodeUsageRetryAfter(raw, now)
		require.False(t, ok, raw)
	}
}

func TestV0215ZhipuMonitorURL(t *testing.T) {
	for base, want := range map[string]string{
		"https://open.bigmodel.cn":             "https://open.bigmodel.cn/api/paas/v4/chat/completions",
		"https://open.bigmodel.cn/api/paas/v4": "https://open.bigmodel.cn/api/paas/v4/chat/completions",
		"https://proxy.example/v1":             "https://proxy.example/v1/chat/completions",
		"https://proxy.example/api/paas/v4":    "https://proxy.example/api/paas/v4/chat/completions",
	} {
		require.Equal(t, want, zhipuMonitorURL(base))
	}
}
