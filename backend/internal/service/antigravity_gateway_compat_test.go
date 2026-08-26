package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAntigravityCompatPendingBufferHasCountAndByteBounds(t *testing.T) {
	session := &antigravityCompatStreamSession{}
	metadata := apicompat.AnthropicStreamEvent{Type: "message_start"}

	session.emitOrBuffer(metadata, antigravityCompatMaxPendingBytes+1)
	require.True(t, session.pendingBufferOverflowed())
	require.Empty(t, session.pendingEvents)
	require.True(t, session.result(false).localCapacityLimited)

	session = &antigravityCompatStreamSession{}
	for range antigravityCompatMaxPendingEvents {
		session.emitOrBuffer(metadata, 1)
	}
	require.False(t, session.pendingBufferOverflowed())
	session.emitOrBuffer(metadata, 1)
	require.True(t, session.pendingBufferOverflowed())
	require.Len(t, session.pendingEvents, antigravityCompatMaxPendingEvents)
	require.True(t, session.result(false).localCapacityLimited)
}

func TestCollectClaudeStreamResponseHonorsTotalResponseLimit(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.UpstreamResponseReadMaxBytes = 32
	svc := &AntigravityGatewayService{settingService: &SettingService{cfg: cfg}}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat(": metadata\n", 4)))}

	_, _, err := svc.collectClaudeStreamResponse(resp, time.Now(), "claude-test")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrUpstreamResponseBodyTooLarge))
}

func TestOpenAIBodyHasThinkingEnabledIgnoresJSONFormatting(t *testing.T) {
	require.True(t, OpenAIBodyHasThinkingEnabled([]byte(`{"model":"gpt-5.6","thinking":{"budget_tokens":2048,"type":"enabled"}}`)))
	require.True(t, OpenAIBodyHasThinkingEnabled([]byte("{\n  \"thinking\": { \"type\": \"adaptive\", \"budget_tokens\": 1024 },\n  \"model\": \"gpt-5.6\"\n}")))
	require.False(t, OpenAIBodyHasThinkingEnabled([]byte(`{"thinking":{"type":"disabled"}}`)))
	require.False(t, OpenAIBodyHasThinkingEnabled([]byte(`{"thinking":"enabled"}`)))
}

func TestPreserveChatCompletionTokenLimitClampsAntigravityMaximum(t *testing.T) {
	tooLarge := 100000
	request := &apicompat.ChatCompletionsRequest{MaxCompletionTokens: &tooLarge}
	claudeRequest := &apicompat.AnthropicRequest{}

	preserveChatCompletionTokenLimit(request, claudeRequest)
	require.Equal(t, antigravityCompatMaxTokens, claudeRequest.MaxTokens)
}

func TestEnableMixedGeminiToolInvocations(t *testing.T) {
	body := []byte(`{"tools":[{"googleSearch":{}},{"functionDeclarations":[{"name":"lookup"}]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`)
	updated, err := enableMixedGeminiToolInvocations(body)
	require.NoError(t, err)
	require.JSONEq(t, `{"tools":[{"googleSearch":{}},{"functionDeclarations":[{"name":"lookup"}]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO"},"includeServerSideToolInvocations":true}}`, string(updated))

	searchOnly := []byte(`{"tools":[{"googleSearch":{}}]}`)
	unchanged, err := enableMixedGeminiToolInvocations(searchOnly)
	require.NoError(t, err)
	require.Equal(t, searchOnly, unchanged)
}

func TestAntigravityCompatReadErrorTreatsCanceledRequestAsNeutral(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = request

	session := &antigravityCompatStreamSession{
		processor: antigravity.NewStreamingProcessor("test-model"),
		usage:     &ClaudeUsage{InputTokens: 12},
	}
	result, err := (&AntigravityGatewayService{}).handleAntigravityCompatReadError(
		ginContext,
		session,
		errors.New("read canceled"),
		defaultMaxLineSize,
		"test",
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.clientDisconnect)
	require.Equal(t, 12, result.usage.InputTokens)
}
