package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/websearch"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV0224MessagesToolSearchUsesOutboundCredential(t *testing.T) {
	account := newCodexModelsTestAccount()
	cachedToken := "refreshed-token"
	tokenProvider := NewOpenAITokenProvider(nil, &forceRefreshCacheStub{
		tokens: map[string]string{OpenAITokenCacheKey(account): cachedToken},
	}, nil)
	const catalog = `{"models":[{"slug":"gpt-5.4","supports_search_tool":true}]}`
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"ToolSearch","input_schema":{"type":"object"}},{"name":"Lookup","defer_loading":true,"input_schema":{"type":"object"}}]}`)
	for _, test := range []struct {
		name, manifestToken, wantToolType string
		wantToolCount                     int
	}{
		{"stale account credential", account.GetOpenAIAccessToken(), "function", 2},
		{"current outbound credential", cachedToken, "tool_search", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &codexModelsMemoryUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				outbound, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, "Bearer "+cachedToken, req.Header.Get("Authorization"))
				require.Equal(t, test.wantToolType, gjson.GetBytes(outbound, "tools.0.type").String())
				require.Len(t, gjson.GetBytes(outbound, "tools").Array(), test.wantToolCount)
				return &http.Response{
					StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n")),
				}, nil
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, openAITokenProvider: tokenProvider}
			svc.codexToolSearchCache.store(account.ID, test.manifestToken, &CodexModelsManifest{Body: []byte(catalog)}, time.Now())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			result, err := svc.ForwardAsAnthropic(t.Context(), c, account, body, "", "")
			require.NoError(t, err)
			require.Equal(t, 5, result.Usage.InputTokens)
		})
	}
}

func TestV0224CodexToolSearchCacheRetainsAPIKeyManifest(t *testing.T) {
	svc := &OpenAIGatewayService{}
	now := time.Now()
	manifest := &CodexModelsManifest{Body: []byte(`{"models":[{"slug":"gpt-5.4","supports_search_tool":true}]}`)}
	svc.codexModelsManifestCache.set("api-key-account", manifest, now)
	for id := int64(1); id <= codexModelsManifestCacheMaxEntries+1; id++ {
		svc.codexToolSearchCache.store(id, "fixture-token", manifest, now.Add(time.Duration(id)*time.Millisecond))
	}
	require.False(t, svc.codexToolSearchCache.supports(1, "fixture-token", "gpt-5.4", now))
	const accountID = codexModelsManifestCacheMaxEntries + 1
	require.True(t, svc.codexToolSearchCache.supports(accountID, "fixture-token", "gpt-5.4", now))
	require.False(t, svc.codexToolSearchCache.supports(accountID, "different-token", "gpt-5.4", now))
	require.False(t, svc.codexToolSearchCache.supports(accountID, "fixture-token", "gpt-5.4", now.Add(time.Minute)))
	cached, state := svc.codexModelsManifestCache.get("api-key-account", now.Add(time.Second))
	require.Same(t, manifest, cached)
	require.Equal(t, codexModelsManifestCacheFresh, state)
}

func TestV0224WebSearchRequiresConfiguredAccountProxy(t *testing.T) {
	proxyID := int64(7)
	_, _, err := doWebSearch(t.Context(), &Account{ProxyID: &proxyID}, "fixture search")
	require.ErrorIs(t, err, websearch.ErrProxyUnavailable)
}

func TestV0224CodexManifestRequiresConfiguredAccountProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("manifest request reached upstream without the configured proxy")
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	originalURL := chatgptCodexModelsURL
	chatgptCodexModelsURL = upstream.URL
	t.Cleanup(func() { chatgptCodexModelsURL = originalURL })

	proxyID := int64(7)
	account := newCodexModelsTestAccount()
	account.ProxyID = &proxyID
	svc := &OpenAIGatewayService{}
	manifest, err := svc.FetchCodexModelsManifest(t.Context(), account, "", "")
	require.Nil(t, manifest)
	require.ErrorContains(t, err, "account proxy is unavailable")
	require.True(t, IsRetryableCodexModelsManifestError(err))
}

func TestV0224CodexAudioProjectionPreservesDirectoryAndRespectsForcedChat(t *testing.T) {
	catalog := convertOpenAIModelListToCodexManifest([]byte(`{"data":[{"id":"broken","input_modalities":"audio"},{"id":"capable","input_modalities":["text","audio"]}]}`))
	require.Equal(t, "broken", gjson.GetBytes(catalog, "models.0.slug").String())
	require.False(t, gjson.GetBytes(catalog, "models.0.input_modalities").Exists())
	require.Equal(t, `["text","audio"]`, gjson.GetBytes(catalog, "models.1.input_modalities").Raw)
	account := newCodexModelsAPIKeyTestAccount("https://fixture.example")
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesSupported: true,
		openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeForceChatCompletions),
	}
	projected, err := projectCodexManifestForAccount(catalog, account)
	require.NoError(t, err)
	require.Equal(t, `["text"]`, gjson.GetBytes(projected, "models.1.input_modalities").Raw)
}

func TestV0224CompositeBedrockUsesResolvedClaudeThinkingContract(t *testing.T) {
	const publicModel = "public-model"
	const target = "us.anthropic.claude-sonnet-5-5-v1:0"
	const oldModel = "us.anthropic.claude-sonnet-4-6"
	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeBedrock, Credentials: map[string]any{
		"auth_mode": "apikey", "api_key": "fixture", "region": "us-east-1",
		"model_mapping": map[string]any{publicModel: oldModel, "route-target": target},
	}}
	for _, test := range []struct {
		name, resolved, sentModel, thinking, wantThinking string
		wantBudget                                        bool
	}{
		{"enabled", "route-target", target, `{"type":"enabled","budget_tokens":1024}`, "adaptive", false},
		{"disabled", "route-target", target, `{"type":"disabled"}`, "between_tools", false},
		{"ordinary Bedrock", "", oldModel, `{"type":"enabled","budget_tokens":1024}`, "enabled", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"model":"public-model","messages":[{"role":"user","content":"hi"}],"max_tokens":2048,"thinking":` + test.thinking + `}`)
			upstream := &codexModelsMemoryUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				providerBody, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Contains(t, req.URL.Path, "/model/"+test.sentModel+"/invoke")
				require.Equal(t, test.wantThinking, gjson.GetBytes(providerBody, "thinking.type").String())
				require.Equal(t, test.wantBudget, gjson.GetBytes(providerBody, "thinking.budget_tokens").Exists())
				return &http.Response{StatusCode: 200, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{"id":"msg_fixture","type":"message","role":"assistant","model":"` + test.sentModel + `","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":6,"output_tokens":3}}`)),
				}, nil
			}}
			svc := &GatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			ctx := WithResolvedUpstreamModel(t.Context(), test.resolved)
			result, err := svc.Forward(ctx, c, account, &ParsedRequest{Model: publicModel, Body: body})
			require.NoError(t, err)
			require.Equal(t, test.sentModel, result.UpstreamModel)
			require.Equal(t, publicModel, result.Model)
			require.Equal(t, 6, result.Usage.InputTokens)
			wantResponseModel := publicModel
			if test.resolved == "" {
				wantResponseModel = test.sentModel
			}
			require.Equal(t, wantResponseModel, gjson.Get(rec.Body.String(), "model").String())
		})
	}
}

func TestV0224PassthroughValidatesBeforeRequestHooks(t *testing.T) {
	hookCalls := 0
	hooks := withPassthroughRequestChecks(&OpenAIWSIngressHooks{
		BeforeRequest: func(int, []byte, string) error {
			hookCalls++
			return nil
		},
	}, nil)
	err := hooks.BeforeRequest(2, []byte(`{"type":"response.create","model":"fixture","model":"fixture"}`), "fixture")
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
	require.Zero(t, hookCalls)
	require.NoError(t, hooks.BeforeRequest(2, []byte(`{"type":"response.create","model":"fixture"}`), "fixture"))
	require.Equal(t, 1, hookCalls)
}

func TestV0224AnthropicModelSelectorPreservesConfiguredRoutes(t *testing.T) {
	at := time.Now().UTC()
	group := &Group{ID: 19, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true}
	account := Account{
		ID: 21, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, UpdatedAt: at,
		Credentials: map[string]any{"model_mapping": map[string]any{"public": "provider"}},
		Extra: map[string]any{AnthropicModelContextWindowsExtraKey: map[string]any{
			"account_revision": at.Format(time.RFC3339Nano),
			"windows":          map[string]any{"provider": float64(1_000_000)},
		}},
	}
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{group.ID: {account}}}
	gateway := &GatewayService{accountRepo: repo}
	ctx := context.WithValue(t.Context(), ctxkey.Group, group)
	require.ElementsMatch(t, []string{"public", "public[1m]"}, gateway.GetAvailableModels(ctx, &group.ID, group.Platform))
	require.Equal(t, "public", gateway.ResolveAnthropicModelSelector(ctx, group, "public[1m]"))
	before := repo.listByGroupCalls.Load()
	require.Equal(t, "public", gateway.ResolveAnthropicModelSelector(ctx, group, "public"))
	require.Equal(t, before, repo.listByGroupCalls.Load())

	t.Run("group fallback preserves existing selector", func(t *testing.T) {
		fallbackID := int64(20)
		for _, fallback := range []Group{
			{ID: group.ID, Platform: group.Platform, FallbackGroupIDOnInvalidRequest: &fallbackID},
			{ID: group.ID, Platform: group.Platform, ClaudeCodeOnly: true, FallbackGroupID: &fallbackID},
		} {
			require.Equal(t, "public[1m]", gateway.ResolveAnthropicModelSelector(ctx, &fallback, "public[1m]"))
		}
	})

	t.Run("explicit fallback model wins", func(t *testing.T) {
		literal := account
		literal.ID = 22
		literal.Credentials = map[string]any{"model_mapping": map[string]any{"public[1m]": "long-provider"}}
		repo.byGroup[group.ID] = []Account{account, literal}
		require.Equal(t, "public[1m]", gateway.ResolveAnthropicModelSelector(ctx, group, "public[1m]"))
	})
	t.Run("mixed capability cannot promise selector", func(t *testing.T) {
		unknown := account
		unknown.ID = 23
		unknown.UpdatedAt = at.Add(time.Second)
		repo.byGroup[group.ID] = []Account{account, unknown}
		require.Equal(t, []string{"public"}, gateway.GetAvailableModels(ctx, &group.ID, group.Platform))
		require.Equal(t, "public[1m]", gateway.ResolveAnthropicModelSelector(ctx, group, "public[1m]"))
	})
}

func TestV0224GrokTerminalUsageTakesPrecedenceOverLaterEvents(t *testing.T) {
	stream := "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"server_side_tool_usage_details\":{\"web_search_calls\":0}}}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"search-fixture\",\"type\":\"web_search_call\",\"status\":\"completed\"}}\n\n"
	require.Zero(t, countGrokNativeSearchCallsFromSSEBody(stream))
}

func TestV0224NonStreamingGrokResponsesPreserveSearchUsage(t *testing.T) {
	const response = `{"id":"resp_1","object":"response","model":"grok-4.5","status":"completed","output":[{"id":"ws_1","type":"web_search_call","status":"completed"}],"usage":{"input_tokens":3,"output_tokens":4,"server_side_tool_usage_details":{"web_search_calls":2,"x_search_calls":1}}}`
	for _, test := range []struct {
		name, contentType, payload string
		count                      int
	}{
		{"JSON reported", "application/json", response, 3},
		{"JSON fallback", "application/json", strings.Replace(response, `,"server_side_tool_usage_details":{"web_search_calls":2,"x_search_calls":1}`, "", 1), 1},
		{"SSE reported", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n", 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {test.contentType}},
				Body: io.NopCloser(strings.NewReader(test.payload)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 77, Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture-token"}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			result, err := svc.Forward(t.Context(), c, account, []byte(`{"model":"grok","input":"hi","stream":false}`))
			require.NoError(t, err)
			require.Equal(t, test.count, result.SearchCount)
			require.Equal(t, 3, result.Usage.InputTokens)
			require.Equal(t, 4, result.Usage.OutputTokens)
		})
	}
}

func TestV0224GrokSearchFallbackCountsOnlyDistinctFinishedItems(t *testing.T) {
	require.Equal(t, 2, countGrokNativeSearchCallsFromJSONBytes([]byte(`{"output":[
		{"type":"web_search_call","id":"done","status":"completed"},
		{"type":"web_search_call","id":"done","status":"completed"},
		{"type":"x_search_call","id":"failed","status":"failed"},
		{"type":"x_search_call","id":"queued","status":"in_progress"},
		{"type":"web_search_call"}
	]}`)))

	stream := `data: {"type":"response.output_item.done","item":{"type":"web_search_call","status":"completed"}}

data: {"type":"response.completed","response":{"output":[{"type":"web_search_call","status":"completed"}]},"output":[{"type":"web_search_call","status":"completed"}]}

`
	require.Equal(t, 1, countGrokNativeSearchCallsFromSSEBody(stream))
}

func TestV0224RawGrokChatKeepsProviderSearchCount(t *testing.T) {
	setGinTestMode()
	for _, tc := range []struct {
		name   string
		stream bool
		count  int
	}{
		{"buffered", false, 3},
		{"streamed", true, 3},
		{"streamed explicit zero", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(`{"model":"grok-fixture","messages":[{"role":"user","content":"hello"}],"stream":%t}`, tc.stream))
			contentType := "application/json"
			usage := fmt.Sprintf(`{"prompt_tokens":5,"completion_tokens":8,"server_side_tool_usage_details":{"web_search_calls":%d}}`, tc.count)
			providerBody := `{"choices":[{"message":{"content":"ok"}}],"usage":` + usage + `}`
			if tc.stream {
				contentType = "text/event-stream"
				providerBody = "data: {\"id\":\"chat-fixture\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
					"data: {\"id\":\"chat-fixture\",\"choices\":[],\"usage\":" + usage + "}\n\ndata: [DONE]\n\n"
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}},
				Body: io.NopCloser(strings.NewReader(providerBody)),
			}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
			gateway := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 103, Platform: PlatformGrok, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://api.x.ai"}}
			result, err := gateway.ForwardAsChatCompletions(t.Context(), c, account, payload, "", "")
			require.NoError(t, err)
			require.Equal(t, tc.count, result.SearchCount)
			require.Equal(t, 5, result.Usage.InputTokens)
			require.Equal(t, 8, result.Usage.OutputTokens)
			require.Contains(t, recorder.Body.String(), usage)
		})
	}
}

func TestOpenAIPassthroughModelRestrictionIsAppliedDuringScheduling(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5.4": "local-alias"}},
		Extra:       map[string]any{"openai_passthrough": true},
	}
	req := OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.3-codex", RequiredCapability: OpenAIEndpointCapabilityChatCompletions}
	require.True(t, openAITextRequestModelSupported(account, req))
	account.Extra["openai_passthrough_model_filter"] = true
	require.False(t, openAITextRequestModelSupported(account, req))
	req.RequestedModel = "gpt-5.4"
	require.True(t, openAITextRequestModelSupported(account, req))
	projected, err := projectCodexManifestForAccount([]byte(`{"models":[{"slug":"gpt-5.4"},{"slug":"gpt-5.3-codex"},{"slug":"local-alias"}]}`), account)
	require.NoError(t, err)
	require.JSONEq(t, `{"models":[{"slug":"gpt-5.4"}]}`, string(projected))
}

func TestOpenAIOAuthSchedulerRejectsForeignPassthroughTarget(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-6": "gpt-5.4"}},
		Extra:       map[string]any{"openai_passthrough": true},
	}
	req := OpenAIAccountScheduleRequest{RequestedModel: "claude-sonnet-4-6", RequiredCapability: OpenAIEndpointCapabilityChatCompletions}
	require.False(t, openAITextRequestModelSupported(account, req))
	account.Extra["openai_passthrough"] = false
	require.True(t, openAITextRequestModelSupported(account, req))
}

func TestClaudeCompactionRecognizesOnlyTheLastUserText(t *testing.T) {
	require.True(t, isClaudeCodeCompactionRequest([]byte(`{"messages":[{"role":"user","content":[
       {"type":"tool_result","content":"ordinary output"},
       {"type":"text","text":"CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\nSummarize this conversation"}
    ]}]}`)))
	require.False(t, isClaudeCodeCompactionRequest([]byte(`{"messages":[{"role":"user","content":[
       {"type":"tool_result","content":"CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\nquoted"}
    ]}]}`)))
}
