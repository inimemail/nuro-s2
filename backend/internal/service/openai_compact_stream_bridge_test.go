package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newCompactBridgeTestContext(t *testing.T, markClientStream bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	if markClientStream {
		MarkOpenAICompactClientStream(c)
	}
	return c, rec
}

func newCompactBridgeTestService() *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg:           &config.Config{},
		toolCorrector: NewCodexToolCorrector(),
	}
}

func parseCompactBridgeSSE(t *testing.T, body string) [][2]string {
	t.Helper()
	var events [][2]string
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		lines := strings.Split(block, "\n")
		require.Len(t, lines, 2, "each SSE event should contain event and data lines: %q", block)
		require.True(t, strings.HasPrefix(lines[0], "event: "), "missing event line: %q", block)
		require.True(t, strings.HasPrefix(lines[1], "data: "), "missing data line: %q", block)
		events = append(events, [2]string{
			strings.TrimPrefix(lines[0], "event: "),
			strings.TrimPrefix(lines[1], "data: "),
		})
	}
	return events
}

func TestBuildOpenAICompactSSEPayload_EmitsItemsAndCompleted(t *testing.T) {
	finalResponse := []byte(`{
		"id":"resp_compact_1",
		"object":"response",
		"model":"gpt-5.1-codex",
		"status":"completed",
		"output":[
			{"id":"cmp_1","type":"compaction","status":"completed","encrypted_content":"compact-payload","summary":[{"type":"summary_text","text":"compact summary"}],"opaque":{"kept":true}},
			{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
		],
		"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}
	}`)

	payload, ok := buildOpenAICompactSSEPayload(finalResponse)
	require.True(t, ok)

	events := parseCompactBridgeSSE(t, string(payload))
	require.Len(t, events, 3)
	require.Equal(t, "response.output_item.done", events[0][0])
	require.Equal(t, int64(0), gjson.Get(events[0][1], "output_index").Int())
	require.Equal(t, "compaction", gjson.Get(events[0][1], "item.type").String())
	require.Equal(t, "compact-payload", gjson.Get(events[0][1], "item.encrypted_content").String())
	require.Equal(t, "compact summary", gjson.Get(events[0][1], "item.summary.0.text").String())
	require.True(t, gjson.Get(events[0][1], "item.opaque.kept").Bool())

	require.Equal(t, "response.output_item.done", events[1][0])
	require.Equal(t, int64(1), gjson.Get(events[1][1], "output_index").Int())
	require.Equal(t, "message", gjson.Get(events[1][1], "item.type").String())

	require.Equal(t, "response.completed", events[2][0])
	require.Equal(t, "response.completed", gjson.Get(events[2][1], "type").String())
	require.Equal(t, "resp_compact_1", gjson.Get(events[2][1], "response.id").String())
	require.Equal(t, int64(13), gjson.Get(events[2][1], "response.usage.total_tokens").Int())
}

func TestBuildOpenAICompactSSEPayload_InjectsMissingResponseID(t *testing.T) {
	payload, ok := buildOpenAICompactSSEPayload([]byte(`{"output":[{"type":"compaction","encrypted_content":"x"}]}`))
	require.True(t, ok)

	events := parseCompactBridgeSSE(t, string(payload))
	require.Len(t, events, 2)
	id := gjson.Get(events[1][1], "response.id").String()
	require.True(t, strings.HasPrefix(id, "resp_"), "missing id should be injected as resp_*: %q", id)
	require.NotEqual(t, "resp_", id)
}

func TestBuildOpenAICompactSSEPayload_DropsMalformedUsage(t *testing.T) {
	payload, ok := buildOpenAICompactSSEPayload([]byte(`{
		"id":"resp_1",
		"output":[{"type":"compaction","encrypted_content":"x"}],
		"usage":{"prompt_tokens":9,"completion_tokens":4}
	}`))
	require.True(t, ok)

	events := parseCompactBridgeSSE(t, string(payload))
	completed := events[len(events)-1][1]
	require.False(t, gjson.Get(completed, "response.usage").Exists())
}

func TestBuildOpenAICompactSSEPayload_KeepsWellFormedUsage(t *testing.T) {
	payload, ok := buildOpenAICompactSSEPayload([]byte(`{
		"id":"resp_1",
		"output":[{"type":"compaction","encrypted_content":"x"}],
		"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13,"input_tokens_details":{"cached_tokens":2}}
	}`))
	require.True(t, ok)

	events := parseCompactBridgeSSE(t, string(payload))
	completed := events[len(events)-1][1]
	require.Equal(t, int64(9), gjson.Get(completed, "response.usage.input_tokens").Int())
	require.Equal(t, int64(2), gjson.Get(completed, "response.usage.input_tokens_details.cached_tokens").Int())
}

func TestBuildOpenAICompactSSEPayload_RejectsNonJSONObject(t *testing.T) {
	for name, body := range map[string][]byte{
		"empty":     nil,
		"sse_text":  []byte("data: {\"type\":\"response.completed\"}\n\n"),
		"array":     []byte(`[{"id":"resp_1"}]`),
		"non_json":  []byte("upstream said no"),
		"bare_true": []byte("true"),
	} {
		_, ok := buildOpenAICompactSSEPayload(body)
		require.False(t, ok, "case %s should not be bridged to SSE", name)
	}
}

func TestWriteOpenAICompactSSEBridge_RequiresMarkAndSuccessStatus(t *testing.T) {
	finalResponse := []byte(`{"id":"resp_1","output":[{"type":"compaction","encrypted_content":"x"}]}`)

	c, rec := newCompactBridgeTestContext(t, false)
	require.False(t, writeOpenAICompactSSEBridge(c, http.StatusOK, finalResponse))
	require.Zero(t, rec.Body.Len())

	c, rec = newCompactBridgeTestContext(t, true)
	require.False(t, writeOpenAICompactSSEBridge(c, http.StatusBadGateway, finalResponse))
	require.Zero(t, rec.Body.Len())

	c, rec = newCompactBridgeTestContext(t, true)
	require.True(t, writeOpenAICompactSSEBridge(c, http.StatusOK, finalResponse))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.Contains(t, rec.Body.String(), "event: response.completed")
}

func TestHandleNonStreamingResponse_CompactClientStreamBridgesToSSE(t *testing.T) {
	svc := newCompactBridgeTestService()
	c, rec := newCompactBridgeTestContext(t, true)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"id":"resp_compact_json",
			"object":"response",
			"model":"gpt-5.1-codex",
			"status":"completed",
			"output":[{"id":"cmp_1","type":"compaction","status":"completed","encrypted_content":"compact-payload"}],
			"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}
		}`)),
	}

	result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, &Account{ID: 1, Type: AccountTypeOAuth}, "gpt-5.5", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))

	events := parseCompactBridgeSSE(t, rec.Body.String())
	require.Len(t, events, 2)
	require.Equal(t, "response.output_item.done", events[0][0])
	require.Equal(t, "compaction", gjson.Get(events[0][1], "item.type").String())
	require.Equal(t, "response.completed", events[1][0])
	require.Equal(t, "resp_compact_json", gjson.Get(events[1][1], "response.id").String())
	require.Equal(t, 9, result.usage.InputTokens)
	require.Equal(t, 4, result.usage.OutputTokens)
	require.Equal(t, "resp_compact_json", result.responseID)
}

func TestHandleNonStreamingResponse_PathBasedCompactStaysJSON(t *testing.T) {
	svc := newCompactBridgeTestService()
	c, rec := newCompactBridgeTestContext(t, false)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"id":"resp_compact_json",
			"output":[{"id":"cmp_1","type":"compaction","encrypted_content":"compact-payload"}],
			"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}
		}`)),
	}

	result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, &Account{ID: 1, Type: AccountTypeOAuth}, "gpt-5.5", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotContains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	require.Equal(t, "resp_compact_json", gjson.Get(rec.Body.String(), "id").String())
	require.Equal(t, "compaction", gjson.Get(rec.Body.String(), "output.0.type").String())
}

func TestHandleSSEToJSON_CompactClientStreamBridgesToSSE(t *testing.T) {
	svc := newCompactBridgeTestService()
	c, rec := newCompactBridgeTestContext(t, true)
	upstreamSSE := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_compact_sse","object":"response","model":"gpt-5.1-codex","status":"completed","output":[{"id":"cmp_sse_1","type":"compaction","status":"completed","encrypted_content":"compact-sse-payload"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}

	result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, &Account{ID: 1, Type: AccountTypeOAuth}, "gpt-5.5", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))

	events := parseCompactBridgeSSE(t, rec.Body.String())
	require.Len(t, events, 2)
	require.Equal(t, "response.output_item.done", events[0][0])
	require.Equal(t, "compact-sse-payload", gjson.Get(events[0][1], "item.encrypted_content").String())
	require.Equal(t, "response.completed", events[1][0])
	require.Equal(t, "resp_compact_sse", gjson.Get(events[1][1], "response.id").String())
}

func TestHandleNonStreamingResponsePassthrough_CompactClientStreamBridgesToSSE(t *testing.T) {
	svc := newCompactBridgeTestService()
	c, rec := newCompactBridgeTestContext(t, true)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"id":"resp_compact_pt",
			"output":[{"id":"cmp_pt_1","type":"compaction","encrypted_content":"compact-pt-payload"}],
			"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}
		}`)),
	}

	result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, &Account{Platform: PlatformOpenAI}, "gpt-5.5", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))

	events := parseCompactBridgeSSE(t, rec.Body.String())
	require.Len(t, events, 2)
	require.Equal(t, "compaction", gjson.Get(events[0][1], "item.type").String())
	require.Equal(t, "resp_compact_pt", gjson.Get(events[1][1], "response.id").String())
	require.Equal(t, 7, result.usage.InputTokens)
}
