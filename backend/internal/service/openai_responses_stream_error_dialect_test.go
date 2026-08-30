package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func responsesRequestContext(t *testing.T, path string) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	return c
}

// A bare tool-call error carries no protocol marker, so the request path is the
// only signal that a Responses client is downstream. This is the shape that made
// clients report "stream disconnected before completion" and reconnect.
func TestResponsesStreamErrorEmitsFailedTerminalEvent(t *testing.T) {
	c := responsesRequestContext(t, "/v1/responses")
	payload := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"No tool call found for function call output with call_id call_1.","param":"input"}}`)
	safe, changed := sanitizeOpenAIStreamEventDataForRequest(c, payload, "error", false)
	require.True(t, changed)
	require.Equal(t, "response.failed", gjson.GetBytes(safe, "type").String())
	require.Equal(t, "failed", gjson.GetBytes(safe, "response.status").String())
	require.True(t, gjson.GetBytes(safe, "response.output").IsArray())
	require.Equal(t, safeUpstreamErrorMessage, gjson.GetBytes(safe, "response.error.message").String())
	require.NotContains(t, string(safe), "call_1")
}

// previous_response_not_found on a tool-call continuation must also terminate
// with response.failed rather than a bare error event.
func TestResponsesPreviousResponseNotFoundEmitsFailedTerminalEvent(t *testing.T) {
	c := responsesRequestContext(t, "/v1/responses")
	payload := []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found."}}`)
	safe, changed := sanitizeOpenAIStreamEventDataForRequest(c, payload, "error", false)
	require.True(t, changed)
	require.Equal(t, "response.failed", gjson.GetBytes(safe, "type").String())
	require.Equal(t, "failed", gjson.GetBytes(safe, "response.status").String())
}

// A response_id on the error must be preserved so the client can correlate the
// terminal event with the response it was streaming.
func TestResponsesStreamErrorKeepsResponseID(t *testing.T) {
	payload := []byte(`{"type":"error","response":{"id":"resp_abc123"},"error":{"type":"server_error","message":"private upstream detail"}}`)
	safe, changed := sanitizeOpenAIStreamEventDataForClient(payload, "error", false)
	require.True(t, changed)
	require.Equal(t, "response.failed", gjson.GetBytes(safe, "type").String())
	require.Equal(t, "resp_abc123", gjson.GetBytes(safe, "response.id").String())
	require.NotContains(t, string(safe), "private upstream detail")
}

// Chat Completions must keep its own error envelope; response.failed is not a
// valid event on that protocol.
func TestChatCompletionsStreamErrorKeepsBareErrorShape(t *testing.T) {
	c := responsesRequestContext(t, "/v1/chat/completions")
	payload := []byte(`{"type":"error","error":{"type":"server_error","message":"private upstream detail"}}`)
	safe, changed := sanitizeOpenAIStreamEventDataForRequest(c, payload, "error", false)
	require.True(t, changed)
	require.Equal(t, "error", gjson.GetBytes(safe, "type").String())
	require.False(t, gjson.GetBytes(safe, "response").Exists())
	require.NotContains(t, string(safe), "private upstream detail")
}

// An unsafe passthrough payload on the Responses protocol must also terminate
// with response.failed rather than a bare error.
func TestUnsafeResponsesPassthroughEmitsFailedTerminalEvent(t *testing.T) {
	payload := []byte(`{"type":"response.output_text.delta","response":{"id":"resp_x"},"detail":"private upstream diagnostic"}`)
	safe, changed := sanitizeOpenAIStreamEventDataForClient(payload, "response.output_text.delta", false)
	require.True(t, changed)
	require.Equal(t, "response.failed", gjson.GetBytes(safe, "type").String())
	require.NotContains(t, string(safe), "private upstream diagnostic")
}

func TestOpenAIPayloadDialectDetection(t *testing.T) {
	require.True(t, openAIPayloadIsResponsesDialect([]byte(`{"type":"response.failed"}`)))
	require.True(t, openAIPayloadIsResponsesDialect([]byte(`{"type":"error","response":{"id":"resp_1"}}`)))
	require.True(t, openAIPayloadIsResponsesDialect([]byte(`{"type":"error","response_id":"resp_1"}`)))
	require.False(t, openAIPayloadIsResponsesDialect([]byte(`{"type":"error","choices":[]}`)))
	require.False(t, openAIPayloadIsResponsesDialect([]byte(`{"type":"error"}`)))
	require.False(t, openAIPayloadIsResponsesDialect([]byte(`not json`)))
	require.False(t, openAIPayloadIsResponsesDialect(nil))
}

func TestOpenAIResponsesRequestPathDetection(t *testing.T) {
	require.True(t, isOpenAIResponsesRequestPath(responsesRequestContext(t, "/v1/responses")))
	require.True(t, isOpenAIResponsesRequestPath(responsesRequestContext(t, "/openai/v1/responses")))
	require.False(t, isOpenAIResponsesRequestPath(responsesRequestContext(t, "/v1/chat/completions")))
}
