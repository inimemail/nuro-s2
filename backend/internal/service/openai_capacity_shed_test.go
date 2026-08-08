package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAICapacityShedErrorIsFailoverBeforeClientOutput(t *testing.T) {
	payload := `{"type":"error","error":{"code":"server_is_overloaded","message":"private upstream text"}}`
	require.True(t, isOpenAIUpstreamCapacityShedEvent([]byte(payload)))
	require.False(t, openAIStreamDataStartsClientOutput(payload, "error"))
	require.False(t, openAIStreamDataStartsClientOutput(`{"type":"error","error":{"code":"rate_limit_exceeded"}}`, "error"))
}

func TestOpenAICapacityShedErrorIsSanitizedForClient(t *testing.T) {
	payload := []byte(`{"type":"error","error":{"code":"slow_down","message":"private upstream text","upstream_host":"secret.example"}}`)
	updated, changed := sanitizeOpenAICapacityShedErrorCodeForClient(payload)
	require.True(t, changed)
	require.Contains(t, string(updated), `"code":"server_error"`)
	safe, changed := sanitizeOpenAIStreamEventDataForClient(payload, "error", true)
	require.True(t, changed)
	require.Contains(t, string(safe), `"message":"Upstream request failed"`)
	require.NotContains(t, string(safe), "private upstream text")
}

func TestOpenAICapacityShedNestedErrorDoesNotLeakUpstreamFields(t *testing.T) {
	payload := []byte(`{"type":"error","response":{"error":{"code":"server_is_overloaded","message":"private upstream text","host":"secret.example"}}}`)
	safe, changed := sanitizeOpenAIStreamEventDataForClient(payload, "error", false)
	require.True(t, changed)
	require.Equal(t, `{"type":"error","error":{"type":"server_error","code":"server_error","message":"Upstream request failed"}}`, string(safe))
}
