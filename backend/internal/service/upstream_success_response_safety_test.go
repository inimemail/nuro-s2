package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGatewayNonStreamingRejectsEmbeddedErrorSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := []byte(`{"type":"error","error":{"message":"private-upstream.example Cloudflare"}}`)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	svc := &GatewayService{rateLimitService: &RateLimitService{}}
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	usage, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, "claude-test", "claude-test")
	require.Nil(t, usage)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.False(t, c.Writer.Written())
	require.NotContains(t, recorder.Body.String(), "private-upstream.example")
	require.NotContains(t, recorder.Body.String(), "Cloudflare")
}

func TestCollectGeminiSSERejectsEmbeddedError(t *testing.T) {
	body := strings.NewReader("data: {\"error\":{\"message\":\"private-upstream.example Cloudflare\"}}\n\ndata: [DONE]\n\n")

	response, usage, neutral, err := collectGeminiSSE(body, false)
	require.Error(t, err)
	require.Nil(t, response)
	require.NotNil(t, usage)
	require.False(t, neutral)
	require.NotContains(t, err.Error(), "private-upstream.example")
	require.NotContains(t, err.Error(), "Cloudflare")
}

func TestCollectGeminiSSERejectsHTML(t *testing.T) {
	response, usage, neutral, err := collectGeminiSSE(strings.NewReader("<!DOCTYPE html><title>private-upstream.example</title>"), false)
	require.Error(t, err)
	require.Nil(t, response)
	require.NotNil(t, usage)
	require.False(t, neutral)
	require.NotContains(t, err.Error(), "private-upstream.example")
}

func TestCollectGeminiSSEMissingTerminalPreservesUsageAsNeutral(t *testing.T) {
	body := strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{\"promptTokenCount\":9,\"candidatesTokenCount\":4}}\n\n")

	response, usage, neutral, err := collectGeminiSSE(body, false)
	require.Error(t, err)
	require.Nil(t, response)
	require.NotNil(t, usage)
	require.Equal(t, 9, usage.InputTokens)
	require.Equal(t, 4, usage.OutputTokens)
	require.True(t, neutral)
}

func TestOpenAIPassthroughResponseRejectsDiagnosticEnvelope(t *testing.T) {
	require.True(t, openAIPassthroughResponseIsUnsafe([]byte(`{"message":"private-provider.example failed"}`)))
	require.True(t, openAIPassthroughResponseIsUnsafe([]byte(`{"detail":"private-provider.example failed"}`)))
	require.True(t, openAIPassthroughResponseIsUnsafe([]byte(`{"status":"failed","usage":{"input_tokens":1}}`)))
	require.True(t, openAIPassthroughResponseIsUnsafe([]byte(`{"provider":"private-provider.example"}`)))
	require.True(t, openAIPassthroughResponseIsUnsafe([]byte(`{"type":"response.completed","response":{"message":"private-provider.example failed"}}`)))
	require.True(t, openAIPassthroughResponseIsUnsafe([]byte(`{"type":"response.completed","response":{"provider":"private-provider.example"}}`)))
	require.True(t, openAIPassthroughResponseIsUnsafe([]byte(`{"data":{"error":{"message":"https://private-provider.example failed"}}}`)))
	require.True(t, openAIPassthroughResponseIsUnsafe([]byte(`{"result":{"status":"failed","provider":"private-provider"}}`)))
	require.False(t, openAIPassthroughResponseIsUnsafe([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[]}`)))
	require.False(t, openAIPassthroughResponseIsUnsafe([]byte(`{"id":"resp_2","object":"response","status":"incomplete","output":[]}`)))
	require.False(t, openAIPassthroughResponseIsUnsafe([]byte(`{"type":"message_start","message":{"id":"msg_1"}}`)))
	require.False(t, openAIPassthroughResponseIsUnsafe([]byte(`{"data":[{"embedding":[0.1,0.2],"index":0}]}`)))
	require.False(t, openAIPassthroughResponseIsUnsafe([]byte(`{"output":"search result https://example.com"}`)))
}

func TestOpenAINonStreamingRejectsDiagnosticEnvelopeWithUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"message":"private-provider.example failed","usage":{"input_tokens":1,"output_tokens":0}}`)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	svc := &OpenAIGatewayService{}

	result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "gpt-test", "gpt-test")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.False(t, c.Writer.Written())
	require.NotContains(t, recorder.Body.String(), "private-provider.example")
}

func TestOpenAISSEToJSONRejectsDiagnosticTerminalResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
	body := []byte("data: {\"type\":\"response.completed\",\"response\":{\"message\":\"private-provider.example failed\",\"usage\":{\"input_tokens\":1}}}\n\n")
	svc := &OpenAIGatewayService{}

	result, err := svc.handleSSEToJSON(resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, body, "gpt-test", "gpt-test")
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, recorder.Body.String(), safeUpstreamErrorMessage)
	require.NotContains(t, recorder.Body.String(), "private-provider.example")
}

func TestOpenAIPassthroughNonStreamingStillConvertsSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
		)),
	}
	svc := &OpenAIGatewayService{}

	result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, "gpt-test", "gpt-test")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "resp_1", gjson.Get(recorder.Body.String(), "id").String())
	require.NotContains(t, recorder.Body.String(), "data:")
}

func TestOpenAINonStreamingFailedPreservesUsageAndSanitizesResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"id":"resp_failed","object":"response","model":"gpt-test","status":"failed","error":{"type":"server_error","message":"private-provider internal failure"},"usage":{"input_tokens":7,"output_tokens":2}}`

	t.Run("standard", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
		svc := &OpenAIGatewayService{}

		result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, "gpt-test", "gpt-test")
		require.Error(t, err)
		require.NotNil(t, result)
		require.Equal(t, "response.failed", result.terminalEventType)
		require.Equal(t, 7, result.InputTokens)
		require.Equal(t, 2, result.OutputTokens)
		require.Equal(t, "resp_failed", gjson.Get(recorder.Body.String(), "id").String())
		require.Equal(t, safeUpstreamErrorMessage, gjson.Get(recorder.Body.String(), "error.message").String())
		require.NotContains(t, recorder.Body.String(), "private-provider")
	})

	t.Run("passthrough", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
		svc := &OpenAIGatewayService{}

		result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, "gpt-test", "gpt-test")
		require.Error(t, err)
		require.NotNil(t, result)
		require.Equal(t, "response.failed", result.terminalEventType)
		require.Equal(t, 7, result.InputTokens)
		require.Equal(t, 2, result.OutputTokens)
		require.Equal(t, "resp_failed", gjson.Get(recorder.Body.String(), "id").String())
		require.Equal(t, safeUpstreamErrorMessage, gjson.Get(recorder.Body.String(), "error.message").String())
		require.NotContains(t, recorder.Body.String(), "private-provider")
	})
}

func TestOpenAISSEFailedPreservesUsageAndSanitizesResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
	body := []byte("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed_sse\",\"object\":\"response\",\"model\":\"gpt-test\",\"status\":\"failed\",\"error\":{\"type\":\"server_error\",\"message\":\"private-provider internal failure\"},\"usage\":{\"input_tokens\":9,\"output_tokens\":3}}}\n\n")
	svc := &OpenAIGatewayService{}

	result, err := svc.handleSSEToJSON(resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, body, "gpt-test", "gpt-test")
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, "response.failed", result.terminalEventType)
	require.Equal(t, 9, result.InputTokens)
	require.Equal(t, 3, result.OutputTokens)
	require.Equal(t, "resp_failed_sse", gjson.Get(recorder.Body.String(), "id").String())
	require.Equal(t, safeUpstreamErrorMessage, gjson.Get(recorder.Body.String(), "error.message").String())
	require.NotContains(t, recorder.Body.String(), "private-provider")
}

func TestOpenAIStreamingFailureCannotBeOverwrittenByCompleted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.failed","response":{"id":"resp_failed_first","status":"failed","error":{"type":"invalid_request_error","message":"private-provider failed"},"usage":{"input_tokens":4,"output_tokens":1}}}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_failed_first","status":"completed","usage":{"input_tokens":7,"output_tokens":3}}}`,
		"",
	}, "\n")

	for _, tc := range []struct {
		name string
		run  func(*OpenAIGatewayService, *gin.Context, *http.Response) (string, OpenAIUsage, error)
	}{
		{
			name: "standard",
			run: func(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response) (string, OpenAIUsage, error) {
				result, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "gpt-test", "gpt-test")
				if result == nil {
					return "", OpenAIUsage{}, err
				}
				return result.terminalEventType, *result.usage, err
			},
		},
		{
			name: "passthrough",
			run: func(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response) (string, OpenAIUsage, error) {
				result, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "gpt-test", "gpt-test")
				if result == nil {
					return "", OpenAIUsage{}, err
				}
				return result.terminalEventType, *result.usage, err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(upstreamBody)),
			}

			terminalType, usage, err := tc.run(&OpenAIGatewayService{}, c, resp)
			require.Error(t, err)
			require.Equal(t, "response.failed", terminalType)
			require.Equal(t, 7, usage.InputTokens)
			require.Equal(t, 3, usage.OutputTokens)
			require.NotContains(t, recorder.Body.String(), "private-provider")
		})
	}
}

func TestOpenAIStreamingNeutralTerminalCannotBeOverwrittenByCompleted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.incomplete","response":{"id":"resp_incomplete_first","status":"incomplete","usage":{"input_tokens":4,"output_tokens":1}}}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_incomplete_first","status":"completed","usage":{"input_tokens":7,"output_tokens":3}}}`,
		"",
	}, "\n")

	for _, tc := range []struct {
		name string
		run  func(*OpenAIGatewayService, *gin.Context, *http.Response) (string, OpenAIUsage, error)
	}{
		{
			name: "standard",
			run: func(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response) (string, OpenAIUsage, error) {
				result, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "gpt-test", "gpt-test")
				if result == nil {
					return "", OpenAIUsage{}, err
				}
				return result.terminalEventType, *result.usage, err
			},
		},
		{
			name: "passthrough",
			run: func(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response) (string, OpenAIUsage, error) {
				result, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "gpt-test", "gpt-test")
				if result == nil {
					return "", OpenAIUsage{}, err
				}
				return result.terminalEventType, *result.usage, err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(upstreamBody)),
			}

			terminalType, usage, err := tc.run(&OpenAIGatewayService{}, c, resp)
			require.NoError(t, err)
			require.Equal(t, "response.incomplete", terminalType)
			require.Equal(t, 7, usage.InputTokens)
			require.Equal(t, 3, usage.OutputTokens)
		})
	}
}

func TestProtocolSuccessValidatorsRejectDiagnosticEnvelope(t *testing.T) {
	diagnostic := []byte(`{"message":"private-provider.example failed"}`)
	require.False(t, anthropicSuccessJSONResponseIsValid(diagnostic))
	require.False(t, geminiNativeResponseIsValid(diagnostic))
	require.True(t, anthropicSuccessJSONResponseIsValid([]byte(`{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":0}}`)))
	require.True(t, geminiNativeResponseIsValid([]byte(`{"candidates":[],"promptFeedback":{"blockReason":"SAFETY"}}`)))
}

func TestPassthroughHeaderHelpersDoNotReintroduceUpstreamIdentity(t *testing.T) {
	src := http.Header{
		"Content-Type": []string{`application/json; profile="https://private-provider.example/schema"`},
		"X-Request-Id": []string{"https://private-provider.example/request/123"},
		"Server":       []string{"private-provider-edge"},
	}

	for _, writeHeaders := range []func(http.Header, http.Header){
		func(dst, src http.Header) { writeOpenAIPassthroughResponseHeaders(dst, src, nil) },
		func(dst, src http.Header) { writeAnthropicPassthroughResponseHeaders(dst, src, nil) },
	} {
		dst := make(http.Header)
		writeHeaders(dst, src)
		require.Empty(t, dst.Get("Content-Type"))
		require.Equal(t, responseheaders.PublicRequestID(src.Get("X-Request-Id")), dst.Get("X-Request-Id"))
		require.Empty(t, dst.Get("Server"))
	}
}

func TestAnthropicStreamRejectsDiagnosticEnvelopeButKeepsMessageStart(t *testing.T) {
	errorEvent := false
	lines, unsafe := sanitizeAnthropicUpstreamSSELine(`data: {"message":"private-provider.example failed"}`, &errorEvent)
	require.True(t, unsafe)
	require.NotContains(t, strings.Join(lines, "\n"), "private-provider.example")
	require.Contains(t, strings.Join(lines, "\n"), safeUpstreamErrorMessage)

	lines, unsafe = sanitizeAnthropicUpstreamSSELine(`data: {"type":"message_start","message":{"id":"msg_1"}}`, &errorEvent)
	require.False(t, unsafe)
	require.Equal(t, []string{`data: {"type":"message_start","message":{"id":"msg_1"}}`}, lines)
}

func TestGatewayAnthropicStreamingDoesNotWriteUpstreamErrorIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, upstreamBody := range []string{
		"data: {\"type\":\"error\",\"error\":{\"message\":\"xAI private-provider.example failed\"}}\n\n",
		"<!DOCTYPE html><title>private-provider.example</title>\n\n",
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		}
		svc := &GatewayService{
			cfg:              &config.Config{},
			rateLimitService: &RateLimitService{},
		}

		result, err := svc.handleStreamingResponse(
			context.Background(), resp, c, &Account{ID: 1, Platform: PlatformAnthropic},
			time.Now(), "claude-test", "claude-test", false,
		)
		require.Error(t, err)
		require.Nil(t, result)
		require.NotContains(t, recorder.Body.String(), "private-provider.example")
		require.NotContains(t, recorder.Body.String(), "xAI")
		require.NotContains(t, recorder.Body.String(), "DOCTYPE")
	}
}

func TestGatewayAnthropicStreamingKeepsNormalContentURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	upstreamBody := strings.Join([]string{
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"https://example.com/result"}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}
	svc := &GatewayService{
		cfg:              &config.Config{},
		rateLimitService: &RateLimitService{},
	}

	result, err := svc.handleStreamingResponse(
		context.Background(), resp, c, &Account{ID: 1, Platform: PlatformAnthropic},
		time.Now(), "claude-test", "claude-test", false,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), "https://example.com/result")
}
