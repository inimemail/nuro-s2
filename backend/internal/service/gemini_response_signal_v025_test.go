package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetectGeminiResponseSignal_InBandErrorAndContentFilter(t *testing.T) {
	t.Run("google error envelope", func(t *testing.T) {
		sig, ok := detectGeminiResponseSignal([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"busy"}}`))
		require.True(t, ok)
		require.Equal(t, geminiSignalError, sig.Kind)
		require.Equal(t, http.StatusTooManyRequests, sig.Status)
	})
	t.Run("prompt blocked", func(t *testing.T) {
		sig, ok := detectGeminiResponseSignal([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
		require.True(t, ok)
		require.Equal(t, geminiSignalPromptBlocked, sig.Kind)
		require.Equal(t, http.StatusBadRequest, sig.Status)
	})
	t.Run("content filter", func(t *testing.T) {
		sig, ok := detectGeminiResponseSignal([]byte(`{"candidates":[{"index":0,"finishReason":"PROHIBITED_CONTENT"}]}`))
		require.True(t, ok)
		require.Equal(t, geminiSignalContentFilter, sig.Kind)
	})
	t.Run("normal stop is not an error", func(t *testing.T) {
		_, ok := detectGeminiResponseSignal([]byte(`{"candidates":[{"finishReason":"STOP"}]}`))
		require.False(t, ok)
	})
}

func TestGeminiEmptyResponseBody(t *testing.T) {
	require.True(t, isGeminiEmptyResponseBody(nil))
	require.True(t, isGeminiEmptyResponseBody([]byte(`{"candidates":[]}`)))
	require.False(t, isGeminiEmptyResponseBody([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`)))
	require.False(t, isGeminiEmptyResponseBody([]byte(`{"candidates":[{"finishReason":"STOP"}]}`)))
}
