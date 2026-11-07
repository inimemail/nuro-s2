//go:build unit

package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ssebudget"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestStreamResourceGuardZeroPreservesIdentity(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	writer := c.Writer
	body := io.NopCloser(strings.NewReader("data: {}\n\n"))
	resp := &http.Response{Body: body}
	g := newStreamResourceGuard(resp, c, &config.Config{}, true, false)
	require.Nil(t, g)
	require.Same(t, writer, c.Writer)
	require.Equal(t, body, resp.Body)
	require.Nil(t, streamResourceQueue(&config.Config{}))
}
func TestStreamResourceGuardNeverPublishesPartialFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("data: " + strings.Repeat("secret", 100) + "\n\n"))}
	cfg := &config.Config{}
	cfg.Gateway.SSEMaxEventBytes = 64
	g := newStreamResourceGuard(resp, c, cfg, true, false)
	_, err := c.Writer.WriteString("event: partial\ndata: ")
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.ErrorIs(t, err, ssebudget.ErrExceeded)
	_, err = c.Writer.WriteString("hidden\n\n")
	require.NoError(t, err)
	g.Finish()
	g.Finish()
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error"))
	require.NotContains(t, rec.Body.String(), "partial")
	require.NotContains(t, rec.Body.String(), "hidden")
}
func TestImagesStreamResourceLimitPreservesLateUsage(t *testing.T) {
	for _, async := range []bool{false, true} {
		cfg := &config.Config{}
		cfg.Gateway.SSEMaxEventBytes = 256
		cfg.Gateway.SSEQueueMaxBytes = 128
		if async {
			cfg.Gateway.ImageStreamKeepaliveInterval = 1
		}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
		raw := "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"" + strings.Repeat("secret", 1000) + "\"}\n\n" +
			"data: {\"type\":\"image_generation.completed\",\"usage\":{\"input_tokens\":123,\"output_tokens\":45}}\n\n"
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(raw))}
		usage, _, _, _, err := (&OpenAIGatewayService{cfg: cfg}).handleOpenAIImagesStreamingResponseForRequest(resp, c, time.Now(), nil, true)
		require.ErrorIs(t, err, ssebudget.ErrExceeded)
		require.Equal(t, 123, usage.InputTokens)
		require.Equal(t, 45, usage.OutputTokens)
		require.NotContains(t, rec.Body.String(), "secret")
		require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error"))
	}
}
func TestAntigravityStreamResourceLimitPreservesLateUsage(t *testing.T) {
	for _, claude := range []bool{false, true} {
		cfg := &config.Config{}
		cfg.Gateway.SSEMaxEventBytes = 512
		cfg.Gateway.SSEQueueMaxBytes = 128
		svc := &AntigravityGatewayService{settingService: &SettingService{cfg: cfg}}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		raw := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"" + strings.Repeat("secret", 1000) + "\"}]}}]}\n\n" +
			"data: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[]}}],\"usageMetadata\":{\"promptTokenCount\":123,\"candidatesTokenCount\":45}}\n\n"
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(raw))}
		var result *antigravityStreamResult
		var err error
		if claude {
			result, err = svc.handleClaudeStreamingResponse(c, resp, time.Now(), "claude-sonnet-4")
		} else {
			result, err = svc.handleGeminiStreamingResponse(c, resp, time.Now())
		}
		require.ErrorIs(t, err, ssebudget.ErrExceeded)
		require.NotNil(t, result)
		require.Equal(t, 123, result.usage.InputTokens)
		require.Equal(t, 45, result.usage.OutputTokens)
		require.NotContains(t, rec.Body.String(), "secret")
		require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error"))
	}
}

func TestGeminiStreamResourceLimitPreservesLateUsage(t *testing.T) {
	for _, mode := range []string{"messages", "native", "chat", "collect", "ag-collect", "ag-claude-collect"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.SSEMaxEventBytes = 512
			cfg.Gateway.SSEQueueMaxBytes = 128
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			raw := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"" + strings.Repeat("secret", 1000) + "\"}]}}]}\n\n" +
				"data: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[]}}],\"usageMetadata\":{\"promptTokenCount\":123,\"candidatesTokenCount\":45}}\n\n" + "data: [DONE]\n\n"
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(raw))}
			svc := &GeminiMessagesCompatService{cfg: cfg}
			var usage *ClaudeUsage
			switch mode {
			case "messages", "chat":
				var result *geminiStreamResult
				var err error
				if mode == "chat" {
					result, err = svc.handleChatCompletionsStreamingResponseFromGemini(c, resp, time.Now(), "gemini-test", false, true)
				} else {
					result, err = svc.handleStreamingResponse(c, resp, time.Now(), "gemini-test")
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, result.failedOutcome)
				usage = result.usage
			case "native":
				result, err := svc.handleNativeStreamingResponse(c, resp, time.Now(), false)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, result.failedOutcome)
				usage = result.usage
			case "collect":
				_, u, _, err := svc.collectGeminiSSEWithResourceLimits(c, resp, false, false)
				require.ErrorIs(t, err, ssebudget.ErrExceeded)
				usage = u
			default:
				ag := &AntigravityGatewayService{settingService: &SettingService{cfg: cfg}}
				var result *antigravityStreamResult
				var err error
				if mode == "ag-collect" {
					result, err = ag.handleGeminiStreamToNonStreaming(c, resp, time.Now())
				} else {
					result, err = ag.handleClaudeStreamToNonStreaming(c, resp, time.Now(), "claude-test")
				}
				require.ErrorIs(t, err, ssebudget.ErrExceeded)
				require.NotNil(t, result)
				usage = result.usage
			}
			require.NotNil(t, usage)
			require.Equal(t, 123, usage.InputTokens)
			require.Equal(t, 45, usage.OutputTokens)
			require.NotContains(t, rec.Body.String(), "secret")
			if strings.Contains(mode, "collect") {
				require.Equal(t, 502, rec.Code)
				require.Equal(t, 1, strings.Count(rec.Body.String(), "exceeded configured"))
			} else {
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error"))
			}
		})
	}
}
