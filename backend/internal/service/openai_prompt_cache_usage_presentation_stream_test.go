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
			{OpenAIPromptCacheCreationOptimizationModeInput125, 5852, 0, "D input_125"},
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
				svc := &OpenAIGatewayService{
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
				require.Equal(t, 4736, downstream.CacheReadInputTokens)
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
		{OpenAIPromptCacheCreationOptimizationModeInput125, 5852, 0},
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
			svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}

			result, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, account, "gpt-5.6-terra", "gpt-5.6-terra")
			require.NoError(t, err)
			require.Equal(t, 5724, result.usage.InputTokens)
			require.Equal(t, 4736, result.usage.CacheReadInputTokens)
			require.Equal(t, 512, result.usage.CacheCreationInputTokens)

			downstream, ok := extractOpenAIUsageFromJSONBytes(recorder.Body.Bytes())
			require.True(t, ok)
			require.Equal(t, tc.wantInput, downstream.InputTokens)
			require.Equal(t, 4736, downstream.CacheReadInputTokens)
			require.Equal(t, tc.wantCreation, downstream.CacheCreationInputTokens)
		})
	}
}

func TestGoNonStreamingDownstreamCacheUsageMode_ExplicitImageRequestIsUnchanged(t *testing.T) {
	setGinTestMode()
	body := `{"id":"resp_image","object":"response","model":"gpt-5.6-terra","status":"completed","output":[],"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_creation_tokens":512}}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}

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
		{OpenAIPromptCacheCreationOptimizationModeInput125, 5852, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, tc.mode)
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}

			t.Run("stream", func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				resp := cacheUsageChatStreamResponse()
				result, err := svc.streamRawChatCompletions(context.Background(), c, resp, account, "gpt-5.6-terra", "gpt-5.6-terra", "gpt-5.6-terra", nil, nil, time.Now(), 1)
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
				require.Equal(t, 4736, downstream.CacheReadInputTokens)
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
		{OpenAIPromptCacheCreationOptimizationModeInput125, 5852, 1116, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, tc.mode)
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}

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
			svc := &OpenAIGatewayService{
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
			require.True(t, gjson.GetBytes(upstream.bodies[0], "prompt_cache_options").Exists())
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
