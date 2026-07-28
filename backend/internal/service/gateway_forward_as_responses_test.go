//go:build unit

package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestExtractResponsesReasoningEffortFromBody(t *testing.T) {
	t.Parallel()

	got := ExtractResponsesReasoningEffortFromBody([]byte(`{"model":"claude-sonnet-4.5","reasoning":{"effort":"HIGH"}}`))
	require.NotNil(t, got)
	require.Equal(t, "high", *got)

	require.Nil(t, ExtractResponsesReasoningEffortFromBody([]byte(`{"model":"claude-sonnet-4.5"}`)))
}

func TestResponsesRequestNeedsAnthropicToolAdaptIsStructural(t *testing.T) {
	require.False(t, responsesRequestNeedsAnthropicToolAdapt([]byte(`{"model":"claude-test","input":"mention the key \"namespace\""}`)))
	require.True(t, responsesRequestNeedsAnthropicToolAdapt([]byte(`{"tools":[{"name":"collaboration","type":"namespace","tools":[]}]}`)))
	require.True(t, responsesRequestNeedsAnthropicToolAdapt([]byte(`{"input":[{"tools":[],"type":"additional_tools"}]}`)))
}

func TestAdaptResponsesRequestForAnthropicPreservesOrdinaryBody(t *testing.T) {
	body := []byte(`{"model":"claude-test","input":"namespace is ordinary prompt text"}`)
	adapted, err := adaptResponsesRequestForAnthropic(nil, body)
	require.NoError(t, err)
	require.Equal(t, body, adapted)
	require.Equal(t, &body[0], &adapted[0], "ordinary requests must retain the original byte slice")
}

func TestHandleResponsesBufferedStreamingResponse_PreservesMessageStartCacheUsage(t *testing.T) {
	t.Parallel()
	setGinTestMode()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_buffered"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":12,"cache_read_input_tokens":9,"cache_creation_input_tokens":3}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}, "\n"))),
	}

	svc := &GatewayService{}
	result, err := svc.handleResponsesBufferedStreamingResponse(resp, c, "claude-sonnet-4.5", "claude-sonnet-4.5", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 7, result.Usage.OutputTokens)
	require.Equal(t, 9, result.Usage.CacheReadInputTokens)
	require.Equal(t, 3, result.Usage.CacheCreationInputTokens)
	require.Contains(t, rec.Body.String(), `"cached_tokens":9`)
}

func TestHandleResponsesStreamingResponse_PreservesMessageStartCacheUsage(t *testing.T) {
	t.Parallel()
	setGinTestMode()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":20,"cache_read_input_tokens":11,"cache_creation_input_tokens":4}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}, "\n"))),
	}

	svc := &GatewayService{}
	result, err := svc.handleResponsesStreamingResponse(resp, c, "claude-sonnet-4.5", "claude-sonnet-4.5", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 20, result.Usage.InputTokens)
	require.Equal(t, 8, result.Usage.OutputTokens)
	require.Equal(t, 11, result.Usage.CacheReadInputTokens)
	require.Equal(t, 4, result.Usage.CacheCreationInputTokens)
	require.Contains(t, rec.Body.String(), `response.completed`)
}

func TestHandleResponsesStreamingResponseUsesSSEEventNameWhenPayloadOmitsType(t *testing.T) {
	t.Parallel()
	setGinTestMode()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_event_name_only"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: message_start`,
			`data: {"message":{"id":"msg_event_name_only","role":"assistant","content":[],"usage":{"input_tokens":3}}}`,
			``,
			`event: content_block_start`,
			`data: {"index":0,"content_block":{"type":"text","text":"hello"}}`,
			``,
			`event: message_delta`,
			`data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			``,
			`event: message_stop`,
			`data: {}`,
			``,
		}, "\n"))),
	}

	result, err := (&GatewayService{}).handleResponsesStreamingResponse(resp, c, "claude-test", "claude-test", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Contains(t, rec.Body.String(), "response.completed")
}

func TestHandleResponsesBufferedStreamingResponseUsesSSEEventNameWhenPayloadOmitsType(t *testing.T) {
	t.Parallel()
	setGinTestMode()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_buffered_event_name_only"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: message_start`,
			`data: {"message":{"id":"msg_buffered_event_name_only","role":"assistant","content":[],"usage":{"input_tokens":4}}}`,
			``,
			`event: message_delta`,
			`data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			``,
			`event: message_stop`,
			`data: {}`,
			``,
		}, "\n"))),
	}

	result, err := (&GatewayService{}).handleResponsesBufferedStreamingResponse(resp, c, "claude-test", "claude-test", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 4, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Contains(t, rec.Body.String(), `"msg_buffered_event_name_only"`)
}

func TestHandleResponsesBufferedStreamingResponse_RejectsMissingMessageStop(t *testing.T) {
	t.Parallel()
	setGinTestMode()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_cut","role":"assistant","content":[],"usage":{"input_tokens":2}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
	}, "\n")))}

	result, err := (&GatewayService{}).handleResponsesBufferedStreamingResponse(resp, c, "claude-test", "claude-test", nil, time.Now())
	require.ErrorContains(t, err, "valid message_stop")
	require.Nil(t, result)
	require.Empty(t, rec.Body.String())
}

func TestHandleResponsesStreamingResponse_WritesSafeFailureForTruncatedStream(t *testing.T) {
	t.Parallel()
	setGinTestMode()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_cut","role":"assistant","content":[],"usage":{"input_tokens":2}}}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"message":"Anthropic https://private-upstream.example failed"}}`,
	}, "\n")))}

	result, err := (&GatewayService{}).handleResponsesStreamingResponse(resp, c, "claude-test", "claude-test", nil, time.Now())
	require.ErrorContains(t, err, "valid message_stop")
	require.NotNil(t, result)
	require.True(t, result.FailedOutcome)
	require.Contains(t, rec.Body.String(), `"type":"response.failed"`)
	require.Contains(t, rec.Body.String(), safeUpstreamErrorMessage)
	require.NotContains(t, rec.Body.String(), "Anthropic")
	require.NotContains(t, rec.Body.String(), "private-upstream.example")
}
