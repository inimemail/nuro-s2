package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAICacheCreationPricing_ForwardingWithoutUpstreamTier(t *testing.T) {
	setGinTestMode()
	for _, viaResponses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(map[bool]string{false: "raw_chat", true: "responses_bridge"}[viaResponses]+map[bool]string{false: "_json", true: "_stream"}[stream], func(t *testing.T) {
				streamValue := "false"
				if stream {
					streamValue = "true"
				}
				body := []byte(`{"model":"gpt-pricing-alias","messages":[{"role":"user","content":"hello"}],"service_tier":"priority","stream":` + streamValue + `}`)
				upstreamJSON := `{"id":"chatcmpl_tier","object":"chat.completion","model":"gpt-pricing-alias","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1300,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":200,"cache_write_tokens":1000}}}`
				contentType := "application/json"
				if viaResponses {
					contentType = "text/event-stream"
					upstreamJSON = "data: " + `{"type":"response.completed","response":{"id":"resp_tier","object":"response","model":"gpt-pricing-alias","status":"completed","output":[{"type":"message","id":"msg_tier","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1300,"output_tokens":200,"input_tokens_details":{"cached_tokens":200,"cache_write_tokens":1000}}}}` + "\n\n"
				} else if stream {
					contentType = "text/event-stream"
					upstreamJSON = "data: " + `{"id":"chatcmpl_tier","object":"chat.completion.chunk","model":"gpt-pricing-alias","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1300,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":200,"cache_write_tokens":1000}}}` + "\n\ndata: [DONE]\n\n"
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(upstreamJSON))}}
				svc := &OpenAIGatewayService{cfg: promptCacheBoostTestConfig(), httpUpstream: upstream, toolCorrector: NewCodexToolCorrector(), billingService: NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
					"gpt-pricing-alias": {InputCostPerToken: 2e-6, OutputCostPerToken: 10e-6, CacheReadInputTokenCost: .2e-6, CacheCreationInputTokenCost: 3e-6,
						InputCostPerTokenPriority: 4e-6, OutputCostPerTokenPriority: 25e-6, CacheReadInputTokenCostPriority: .4e-6, CacheCreationInputTokenCostPriority: 7e-6},
				}})}
				account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
				account.ID, account.Concurrency = 903, 1
				account.Credentials["api_key"] = "sk-test"
				account.Credentials["base_url"] = "https://api.openai.com/v1"
				account.Extra = map[string]any{"openai_responses_supported": viaResponses}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
				result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
				require.NoError(t, err)
				require.NotNil(t, result)
				require.NotNil(t, result.ServiceTier)
				require.Equal(t, "priority", *result.ServiceTier)
				require.Equal(t, 1300, result.Usage.InputTokens)
				require.Equal(t, 1000, result.Usage.CacheCreationInputTokens)
				var downstream OpenAIUsage
				if stream {
					forEachOpenAISSEDataPayload(recorder.Body.String(), func(data []byte) {
						if parsed, ok := extractOpenAIUsageFromJSONBytes(data); ok {
							downstream = parsed
						}
					})
				} else {
					var ok bool
					downstream, ok = extractOpenAIUsageFromJSONBytes(recorder.Body.Bytes())
					require.True(t, ok)
				}
				require.Equal(t, 2033, downstream.InputTokens, "omitted response tier must retain priority request pricing")
				require.Zero(t, downstream.CacheCreationInputTokens)
			})
		}
	}
}

func TestGoStreamingDownstreamCacheUsageModes_PreserveInternalTruth(t *testing.T) {
	setGinTestMode()
	for _, passthrough := range []bool{false, true} {
		for _, tc := range []struct {
			mode             string
			wantInput        int
			wantCreation     int
			wantInternalMode string
		}{
			{OpenAIPromptCacheCreationOptimizationModeSuppress, 5724, 512, "B no-op"},
			{OpenAIPromptCacheCreationOptimizationModeFree, 5212, 0, "C free"},
			{OpenAIPromptCacheCreationOptimizationModeInput125, 5858, 0, "D input_125"},
		} {
			t.Run(tc.wantInternalMode+map[bool]string{false: "_responses", true: "_raw"}[passthrough], func(t *testing.T) {
				upstreamBody := strings.Join([]string{
					`data: {"type":"response.output_text.delta","delta":"ok"}`,
					"",
					`data: {"type":"response.completed","response":{"id":"resp_usage","object":"response","model":"gpt-5.6-terra","status":"completed","output":[],"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_write_tokens":512}}}}`,
					"",
				}, "\n")
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(upstreamBody)),
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, tc.mode)
				account.ID = 395
				svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil),
					cfg:           &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
					toolCorrector: NewCodexToolCorrector(),
				}

				var internal *OpenAIUsage
				if passthrough {
					result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6-terra", "gpt-5.6-terra")
					require.NoError(t, err)
					internal = result.usage
				} else {
					result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6-terra", "gpt-5.6-terra")
					require.NoError(t, err)
					internal = result.usage
				}
				require.Equal(t, 5724, internal.InputTokens)
				require.Equal(t, 4736, internal.CacheReadInputTokens)
				require.Equal(t, 512, internal.CacheCreationInputTokens)

				var downstream OpenAIUsage
				forEachOpenAISSEDataPayload(recorder.Body.String(), func(data []byte) {
					if usage, ok := extractOpenAIUsageFromJSONBytes(data); ok {
						downstream = usage
					}
				})
				require.Equal(t, tc.wantInput, downstream.InputTokens)
				if tc.mode == OpenAIPromptCacheCreationOptimizationModeInput125 {
					require.Equal(t, 4756, downstream.CacheReadInputTokens)
					require.Equal(t, 22, downstream.OutputTokens)
				} else {
					require.Equal(t, 4736, downstream.CacheReadInputTokens)
				}
				require.Equal(t, tc.wantCreation, downstream.CacheCreationInputTokens)
			})
		}
	}
}

func TestGoNonStreamingDownstreamCacheUsageModes_PreserveInternalTruth(t *testing.T) {
	setGinTestMode()
	for _, tc := range []struct {
		mode         string
		wantInput    int
		wantCreation int
	}{
		{OpenAIPromptCacheCreationOptimizationModeSuppress, 5724, 512},
		{OpenAIPromptCacheCreationOptimizationModeFree, 5212, 0},
		{OpenAIPromptCacheCreationOptimizationModeInput125, 5858, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			body := `{"id":"resp_usage","object":"response","model":"gpt-5.6-terra","status":"completed","output":[],"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_creation_tokens":512}}}`
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, tc.mode)
			account.ID = 395
			svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil), cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}

			result, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, account, "gpt-5.6-terra", "gpt-5.6-terra")
			require.NoError(t, err)
			require.Equal(t, 5724, result.usage.InputTokens)
			require.Equal(t, 4736, result.usage.CacheReadInputTokens)
			require.Equal(t, 512, result.usage.CacheCreationInputTokens)

			downstream, ok := extractOpenAIUsageFromJSONBytes(recorder.Body.Bytes())
			require.True(t, ok)
			require.Equal(t, tc.wantInput, downstream.InputTokens)
			if tc.mode == OpenAIPromptCacheCreationOptimizationModeInput125 {
				require.Equal(t, 4756, downstream.CacheReadInputTokens)
				require.Equal(t, 22, downstream.OutputTokens)
			} else {
				require.Equal(t, 4736, downstream.CacheReadInputTokens)
			}
			require.Equal(t, tc.wantCreation, downstream.CacheCreationInputTokens)
		})
	}
}

func TestCFreeNonStreamingPassthroughAndSSEBridge_ZeroDownstreamCreation(t *testing.T) {
	setGinTestMode()
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeFree)
	account.ID = 395
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil), cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}

	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		passthrough bool
	}{
		{
			name:        "json_passthrough",
			contentType: "application/json",
			body:        `{"id":"resp_usage","object":"response","model":"gpt-5.6-terra","status":"completed","output":[],"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_creation_tokens":512}}}`,
			passthrough: true,
		},
		{
			name:        "sse_to_json",
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_usage\",\"object\":\"response\",\"model\":\"gpt-5.6-terra\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":5724,\"output_tokens\":20,\"total_tokens\":5744,\"input_tokens_details\":{\"cached_tokens\":4736,\"cache_creation_tokens\":512}}}}\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{tc.contentType}},
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}

			var internal *OpenAIUsage
			if tc.passthrough {
				result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "gpt-5.6-terra", "gpt-5.6-terra")
				require.NoError(t, err)
				internal = result.usage
			} else {
				result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, "gpt-5.6-terra", "gpt-5.6-terra")
				require.NoError(t, err)
				internal = result.usage
			}

			require.NotNil(t, internal)
			require.Equal(t, 5724, internal.InputTokens)
			require.Equal(t, 512, internal.CacheCreationInputTokens)
			downstream, ok := extractOpenAIUsageFromJSONBytes(recorder.Body.Bytes())
			require.True(t, ok)
			require.Equal(t, 5212, downstream.InputTokens)
			require.Equal(t, 4736, downstream.CacheReadInputTokens)
			require.Zero(t, downstream.CacheCreationInputTokens)
		})
	}
}

func TestResponsesToChatBufferedDownstreamCacheUsageModes_PreserveInternalTruth(t *testing.T) {
	setGinTestMode()
	for _, tc := range []struct {
		mode             string
		wantInput        int
		wantOutput       int
		wantCacheRead    int
		wantCacheCreated int
	}{
		{OpenAIPromptCacheCreationOptimizationModeSuppress, 5724, 20, 4736, 512},
		{OpenAIPromptCacheCreationOptimizationModeFree, 5212, 20, 4736, 0},
		{OpenAIPromptCacheCreationOptimizationModeInput125, 5858, 22, 4756, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			upstreamBody := strings.Join([]string{
				`data: {"type":"response.completed","response":{"id":"resp_usage","object":"response","model":"gpt-5.6-terra","status":"completed","output":[],"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_creation_tokens":512}}}}`,
				"",
				"data: [DONE]",
				"",
			}, "\n")
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(upstreamBody)),
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, tc.mode)
			svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil)}

			result, err := svc.handleChatBufferedStreamingResponse(
				context.Background(), resp, c, account,
				"gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", time.Now(),
			)
			require.NoError(t, err)
			require.Equal(t, 5724, result.Usage.InputTokens)
			require.Equal(t, 20, result.Usage.OutputTokens)
			require.Equal(t, 4736, result.Usage.CacheReadInputTokens)
			require.Equal(t, 512, result.Usage.CacheCreationInputTokens)

			downstream, ok := extractOpenAIUsageFromJSONBytes(recorder.Body.Bytes())
			require.True(t, ok)
			require.Equal(t, tc.wantInput, downstream.InputTokens)
			require.Equal(t, tc.wantOutput, downstream.OutputTokens)
			require.Equal(t, tc.wantCacheRead, downstream.CacheReadInputTokens)
			require.Equal(t, tc.wantCacheCreated, downstream.CacheCreationInputTokens)
		})
	}
}

func TestMessagesBufferedCFree_ZeroesDownstreamCreationAndPreservesInternalTruth(t *testing.T) {
	setGinTestMode()
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_usage","object":"response","model":"gpt-5.6-terra","status":"completed","output":[],"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_creation_tokens":512}}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeFree)
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil)}

	result, err := svc.handleAnthropicBufferedStreamingResponse(
		context.Background(), resp, c, account,
		"gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", time.Now(),
	)
	require.NoError(t, err)
	require.Equal(t, 5724, result.Usage.InputTokens)
	require.Equal(t, 4736, result.Usage.CacheReadInputTokens)
	require.Equal(t, 512, result.Usage.CacheCreationInputTokens)
	require.Equal(t, int64(476), gjson.GetBytes(recorder.Body.Bytes(), "usage.input_tokens").Int())
	require.Equal(t, int64(4736), gjson.GetBytes(recorder.Body.Bytes(), "usage.cache_read_input_tokens").Int())
	require.Zero(t, gjson.GetBytes(recorder.Body.Bytes(), "usage.cache_creation_input_tokens").Int())
}

func TestResponsesToChatBufferedDownstreamCacheUsage_ExplicitImageIsNoOp(t *testing.T) {
	setGinTestMode()
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_usage","object":"response","model":"gpt-5.6-terra","status":"completed","output":[],"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_creation_tokens":512}}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeFree)
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil)}

	result, err := svc.handleChatBufferedStreamingResponse(
		WithOpenAIImageGenerationIntent(context.Background()), resp, c, account,
		"gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", time.Now(),
	)
	require.NoError(t, err)
	require.Equal(t, 512, result.Usage.CacheCreationInputTokens)
	downstream, ok := extractOpenAIUsageFromJSONBytes(recorder.Body.Bytes())
	require.True(t, ok)
	require.Equal(t, 5724, downstream.InputTokens)
	require.Equal(t, 4736, downstream.CacheReadInputTokens)
	require.Equal(t, 512, downstream.CacheCreationInputTokens)
}

func TestResponsesToChatStreamingDownstreamCacheUsageModes_PreserveInternalTruth(t *testing.T) {
	setGinTestMode()
	for _, tc := range []struct {
		mode             string
		wantInput        int
		wantOutput       int
		wantCacheRead    int
		wantCacheCreated int
	}{
		{OpenAIPromptCacheCreationOptimizationModeSuppress, 5724, 20, 4736, 512},
		{OpenAIPromptCacheCreationOptimizationModeFree, 5212, 20, 4736, 0},
		{OpenAIPromptCacheCreationOptimizationModeInput125, 5858, 22, 4756, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			upstreamBody := strings.Join([]string{
				`data: {"type":"response.created","response":{"id":"resp_usage","model":"gpt-5.6-terra"}}`,
				"",
				`data: {"type":"response.completed","response":{"id":"resp_usage","object":"response","model":"gpt-5.6-terra","status":"completed","output":[],"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_creation_tokens":512}}}}`,
				"",
			}, "\n")
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(upstreamBody)),
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, tc.mode)
			svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil)}

			result, err := svc.handleChatStreamingResponse(
				context.Background(), resp, c, account,
				"gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", time.Now(), 0,
			)
			require.NoError(t, err)
			require.Equal(t, 5724, result.Usage.InputTokens)
			require.Equal(t, 20, result.Usage.OutputTokens)
			require.Equal(t, 4736, result.Usage.CacheReadInputTokens)
			require.Equal(t, 512, result.Usage.CacheCreationInputTokens)

			var downstream OpenAIUsage
			forEachOpenAISSEDataPayload(recorder.Body.String(), func(data []byte) {
				if parsed, ok := extractOpenAIUsageFromJSONBytes(data); ok {
					downstream = parsed
				}
			})
			require.Equal(t, tc.wantInput, downstream.InputTokens)
			require.Equal(t, tc.wantOutput, downstream.OutputTokens)
			require.Equal(t, tc.wantCacheRead, downstream.CacheReadInputTokens)
			require.Equal(t, tc.wantCacheCreated, downstream.CacheCreationInputTokens)
		})
	}
}

func TestGoNonStreamingDownstreamCacheUsageMode_ExplicitImageRequestIsUnchanged(t *testing.T) {
	setGinTestMode()
	body := `{"id":"resp_image","created_at":1700000000,"object":"response","model":"gpt-5.6-terra","status":"completed","output":[],"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_creation_tokens":512}}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil), cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}

	ctx := WithOpenAIImageGenerationIntent(context.Background())
	result, err := svc.handleNonStreamingResponse(ctx, resp, c, account, "gpt-5.6-terra", "gpt-5.6-terra")
	require.NoError(t, err)
	require.Equal(t, 512, result.usage.CacheCreationInputTokens)
	require.JSONEq(t, body, recorder.Body.String())
}

func TestRawChatDownstreamCacheUsageModes_PreserveInternalTruth(t *testing.T) {
	setGinTestMode()
	for _, tc := range []struct {
		mode         string
		wantInput    int
		wantCreation int
	}{
		{OpenAIPromptCacheCreationOptimizationModeSuppress, 5724, 512},
		{OpenAIPromptCacheCreationOptimizationModeFree, 5212, 0},
		{OpenAIPromptCacheCreationOptimizationModeInput125, 5858, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, tc.mode)
			svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil), cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}

			t.Run("stream", func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				resp := cacheUsageChatStreamResponse()
				result, err := svc.streamRawChatCompletions(context.Background(), c, resp, account, "gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", nil, nil, time.Now(), 1, openAIRequestFirstTokenPlaceholderState{})
				require.NoError(t, err)
				require.Equal(t, 5724, result.Usage.InputTokens)
				require.Equal(t, 4736, result.Usage.CacheReadInputTokens)
				require.Equal(t, 512, result.Usage.CacheCreationInputTokens)

				var downstream OpenAIUsage
				forEachOpenAISSEDataPayload(recorder.Body.String(), func(data []byte) {
					if parsed, ok := extractOpenAIUsageFromJSONBytes(data); ok {
						downstream = parsed
					}
				})
				require.Equal(t, tc.wantInput, downstream.InputTokens)
				if tc.mode == OpenAIPromptCacheCreationOptimizationModeInput125 {
					require.Equal(t, 4756, downstream.CacheReadInputTokens)
					require.Equal(t, 22, downstream.OutputTokens)
				} else {
					require.Equal(t, 4736, downstream.CacheReadInputTokens)
				}
				require.Equal(t, tc.wantCreation, downstream.CacheCreationInputTokens)
			})

			t.Run("non_stream", func(t *testing.T) {
				body := cacheUsageChatCompletionBody()
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
				result, err := svc.bufferRawChatCompletions(context.Background(), c, resp, account, "gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", nil, nil, time.Now())
				require.NoError(t, err)
				require.Equal(t, 5724, result.Usage.InputTokens)
				require.Equal(t, 512, result.Usage.CacheCreationInputTokens)
				downstream, ok := extractOpenAIUsageFromJSONBytes(recorder.Body.Bytes())
				require.True(t, ok)
				require.Equal(t, tc.wantInput, downstream.InputTokens)
				require.Equal(t, tc.wantCreation, downstream.CacheCreationInputTokens)
				if tc.mode == OpenAIPromptCacheCreationOptimizationModeSuppress {
					require.Equal(t, body, recorder.Body.String(), "B must remain byte-exact")
				}
			})
		})
	}
}

func TestChatFallbackDownstreamCacheUsageModes_PreserveInternalTruth(t *testing.T) {
	setGinTestMode()
	for _, tc := range []struct {
		mode          string
		wantResponses int
		wantAnthropic int
		wantCreation  int
	}{
		{OpenAIPromptCacheCreationOptimizationModeSuppress, 5724, 476, 512},
		{OpenAIPromptCacheCreationOptimizationModeFree, 5212, 476, 0},
		{OpenAIPromptCacheCreationOptimizationModeInput125, 5858, 1102, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, tc.mode)
			svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil), cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}

			t.Run("responses_non_stream", func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				resp := cacheUsageChatCompletionResponse()
				result, err := svc.bufferChatCompletionsAsResponses(context.Background(), c, resp, account, "gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", nil, nil, nil, false, nil, time.Now())
				require.NoError(t, err)
				require.Equal(t, 5724, result.Usage.InputTokens)
				require.Equal(t, 512, result.Usage.CacheCreationInputTokens)
				downstream, ok := extractOpenAIUsageFromJSONBytes(recorder.Body.Bytes())
				require.True(t, ok)
				require.Equal(t, tc.wantResponses, downstream.InputTokens)
				require.Equal(t, tc.wantCreation, downstream.CacheCreationInputTokens)
			})

			t.Run("responses_stream", func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				result, err := svc.streamChatCompletionsAsResponses(context.Background(), c, cacheUsageChatStreamResponse(), account, "gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", nil, nil, nil, false, nil, time.Now())
				require.NoError(t, err)
				require.Equal(t, 5724, result.Usage.InputTokens)
				require.Equal(t, 512, result.Usage.CacheCreationInputTokens)
				var downstream OpenAIUsage
				forEachOpenAISSEDataPayload(recorder.Body.String(), func(data []byte) {
					if parsed, ok := extractOpenAIUsageFromJSONBytes(data); ok {
						downstream = parsed
					}
				})
				require.Equal(t, tc.wantResponses, downstream.InputTokens)
				require.Equal(t, tc.wantCreation, downstream.CacheCreationInputTokens)
			})

			t.Run("messages_non_stream", func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				result, err := svc.bufferChatCompletionsAsAnthropic(context.Background(), c, cacheUsageChatCompletionResponse(), account, "gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", nil, nil, nil, false, nil, time.Now())
				require.NoError(t, err)
				require.Equal(t, 5724, result.Usage.InputTokens)
				require.Equal(t, 512, result.Usage.CacheCreationInputTokens)
				require.Equal(t, int64(tc.wantAnthropic), gjson.GetBytes(recorder.Body.Bytes(), "usage.input_tokens").Int())
				require.Equal(t, int64(tc.wantCreation), gjson.GetBytes(recorder.Body.Bytes(), "usage.cache_creation_input_tokens").Int())
			})

			t.Run("messages_stream", func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				result, err := svc.streamChatCompletionsAsAnthropic(context.Background(), c, cacheUsageChatStreamResponse(), account, "gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", nil, nil, nil, false, nil, time.Now())
				require.NoError(t, err)
				require.Equal(t, 5724, result.Usage.InputTokens)
				require.Equal(t, 512, result.Usage.CacheCreationInputTokens)
				var input, creation int64
				forEachOpenAISSEDataPayload(recorder.Body.String(), func(data []byte) {
					if gjson.GetBytes(data, "type").String() == "message_delta" {
						input = gjson.GetBytes(data, "usage.input_tokens").Int()
						creation = gjson.GetBytes(data, "usage.cache_creation_input_tokens").Int()
					}
				})
				require.Equal(t, int64(tc.wantAnthropic), input)
				require.Equal(t, int64(tc.wantCreation), creation)
			})
		})
	}
}

func TestChatFallbackUnsupportedOptimization_RetriesSameAccountWithoutPolicy(t *testing.T) {
	setGinTestMode()
	for _, protocol := range []string{"responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Unknown parameter: prompt_cache_options"}}`))},
				cacheUsageChatCompletionResponse(),
			}}
			svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil),
				cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
					Enabled: false, AllowInsecureHTTP: true,
				}}},
				httpUpstream: upstream,
			}
			account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeFree)
			account.ID = 395
			account.Credentials["api_key"] = "sk-test"
			account.Credentials["base_url"] = "http://upstream.example"
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)

			var result *OpenAIForwardResult
			var err error
			if protocol == "responses" {
				body := []byte(`{"model":"gpt-5.6-terra","input":"hello","stream":false}`)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
				result, err = svc.forwardResponsesViaRawChatCompletions(context.Background(), c, account, body)
			} else {
				body := []byte(`{"model":"gpt-5.6-terra","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
				result, err = svc.forwardAnthropicViaRawChatCompletions(context.Background(), c, account, body, "")
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 512, result.Usage.CacheCreationInputTokens)
			require.Len(t, upstream.bodies, 2)
			require.Equal(t, "explicit", gjson.GetBytes(upstream.bodies[0], "prompt_cache_options.mode").String())
			require.Equal(t, "24h", gjson.GetBytes(upstream.bodies[0], "prompt_cache_options.ttl").String())
			require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_options").Exists())
			require.Zero(t, gjson.GetBytes(recorder.Body.Bytes(), "usage.cache_creation_input_tokens").Int())
		})
	}
}

func cacheUsageChatCompletionBody() string {
	return `{"id":"chatcmpl_usage","object":"chat.completion","model":"gpt-5.6-terra","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5724,"completion_tokens":20,"total_tokens":5744,"prompt_tokens_details":{"cached_tokens":4736,"cache_write_tokens":512}}}`
}

func cacheUsageChatCompletionResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(cacheUsageChatCompletionBody()))}
}

func cacheUsageChatStreamResponse() *http.Response {
	body := strings.Join([]string{
		`data: {"id":"chatcmpl_usage","object":"chat.completion.chunk","model":"gpt-5.6-terra","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_usage","object":"chat.completion.chunk","model":"gpt-5.6-terra","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl_usage","object":"chat.completion.chunk","model":"gpt-5.6-terra","choices":[],"usage":{"prompt_tokens":5724,"completion_tokens":20,"total_tokens":5744,"prompt_tokens_details":{"cached_tokens":4736,"cache_write_tokens":512}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}
