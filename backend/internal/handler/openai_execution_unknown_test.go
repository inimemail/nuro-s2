package handler

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGuardOpenAIExecutionUnknownSwitchBoundsPoolReplacement(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true}}
	err := &service.UpstreamFailoverError{ExecutionUnknown: true}
	// The first unknown attempt gets one controlled replacement window.
	require.True(t, guardOpenAIExecutionUnknownSwitch(c, context.Background(), account, err))
	// A second unknown attempt is terminal instead of another full POST.
	require.False(t, guardOpenAIExecutionUnknownSwitch(c, context.Background(), account, err))
}

func TestGuardOpenAIExecutionUnknownSwitchKeepsOrdinaryAccountFailover(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Credentials: map[string]any{}}
	err := &service.UpstreamFailoverError{ExecutionUnknown: true}
	require.True(t, guardOpenAIExecutionUnknownSwitch(c, context.Background(), account, err))
	require.True(t, guardOpenAIExecutionUnknownSwitch(c, context.Background(), account, err))
}

func TestApplyConservativeOpenAIMissingUsage(t *testing.T) {
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI}
	result := &service.OpenAIForwardResult{
		AttemptID: "attempt-success", UpstreamRequestBodyStarted: true, Model: "gpt-5.6",
	}
	require.True(t, applyConservativeOpenAIMissingUsage(result, account, []byte(`{"model":"gpt-5.6","max_output_tokens":8192}`), "/v1/responses"))
	require.Greater(t, result.Usage.InputTokens, 0)
	require.Equal(t, 8192, result.Usage.OutputTokens)
	require.False(t, applyConservativeOpenAIMissingUsage(result, account, []byte(`{}`), "/v1/responses"), "existing usage must not be replaced")
}

func TestApplyConservativeOpenAIMissingUsageScopesProviderAndEndpoint(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-large","input":"hello"}`)
	embedding := &service.OpenAIForwardResult{AttemptID: "embedding", UpstreamRequestBodyStarted: true, Model: "text-embedding-3-large"}
	require.True(t, applyConservativeOpenAIMissingUsage(embedding, &service.Account{Platform: service.PlatformOpenAI}, body, "/v1/embeddings"))
	require.Greater(t, embedding.Usage.InputTokens, 0)
	require.Zero(t, embedding.Usage.OutputTokens)

	grok := &service.OpenAIForwardResult{AttemptID: "grok", UpstreamRequestBodyStarted: true, Model: "grok-4"}
	require.False(t, applyConservativeOpenAIMissingUsage(grok, &service.Account{Platform: service.PlatformGrok}, body, "/v1/responses"))
	require.Zero(t, grok.Usage.InputTokens)

	image := &service.OpenAIForwardResult{AttemptID: "image", UpstreamRequestBodyStarted: true, Model: "gpt-image-2"}
	require.True(t, applyConservativeOpenAIMissingUsage(image, &service.Account{Platform: service.PlatformOpenAI}, []byte(`{"model":"gpt-image-2","prompt":"x"}`), "/v1/images/generations"))
	require.Equal(t, 1, image.ImageCount)
}
