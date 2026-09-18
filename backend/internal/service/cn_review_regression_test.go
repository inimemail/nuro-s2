//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCNReviewNativeBlockLifecycle(t *testing.T) {
	start := `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"
	delta := `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n"
	stop := `data: {"type":"message_stop"}` + "\n\n"
	for name, body := range map[string]string{
		"unclosed":    cnBroadStart + start + delta + stop,
		"overlapping": cnBroadStart + start + `data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" + stop,
	} {
		t.Run(name, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
			completed := false
			err := svc.scanNativeAnthropicEvents(&http.Response{Body: io.NopCloser(strings.NewReader(body))}, func(event *apicompat.AnthropicStreamEvent) error {
				completed = completed || event.Type == "message_stop"
				return nil
			})
			require.Error(t, err)
			require.False(t, completed)
		})
	}
}

func TestCNReviewChatBridgeDisconnectFailureKeepsUsageAndMetadata(t *testing.T) {
	for _, mode := range []string{"responses", "messages"} {
		t.Run(mode, func(t *testing.T) {
			setGinTestMode()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/"+mode, nil)
			c.Writer = &openAIChatFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
			// The downstream disconnects, then the upstream sends usage but no
			// terminal event. Both facts must survive in the forwarding result.
			body := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n"
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
			var result *OpenAIForwardResult
			var err error
			if mode == "responses" {
				result, err = svc.streamChatCompletionsAsResponses(context.Background(), c, resp, rawChatCompletionsTestAccount(), "test", "test", "test", nil, nil, nil, false, nil, time.Now())
			} else {
				result, err = svc.streamChatCompletionsAsAnthropic(context.Background(), c, resp, rawChatCompletionsTestAccount(), "test", "test", "test", nil, nil, nil, false, nil, time.Now())
			}
			require.Error(t, err)
			require.NotNil(t, result)
			require.Equal(t, 7, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			require.True(t, result.ClientDisconnect)
			require.Equal(t, "error", result.TerminalEventType)
		})
	}
}

func TestCNReviewNativeEmptyDeltaKeepsInitialArguments(t *testing.T) {
	body := cnBroadStart + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c1","name":"exec","input":{"cmd":"pwd"}}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}` + "\n\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" + `data: {"type":"message_stop"}` + "\n\n"
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	result, _, err := svc.readNativeAnthropicResponse(&http.Response{Body: io.NopCloser(strings.NewReader(body))})
	require.NoError(t, err)
	require.Len(t, result.Content, 1)
	require.JSONEq(t, `{"cmd":"pwd"}`, string(result.Content[0].Input))
}

func TestCNReviewResponsesFailureSequence(t *testing.T) {
	for _, native := range []bool{false, true} {
		setGinTestMode()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		body := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"
		if native {
			body = cnBroadStart
		}
		resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
		var err error
		if native {
			_, err = svc.handleResponsesStreamingFromNativeAnthropic(resp, c, "test", "test", "test", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
		} else {
			_, err = svc.streamChatCompletionsAsResponses(context.Background(), c, resp, rawChatCompletionsTestAccount(), "test", "test", "test", nil, nil, nil, false, nil, time.Now())
		}
		require.Error(t, err)
		previous := int64(-1)
		failed := false
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			payload, ok := extractOpenAISSEDataLine(line)
			if !ok {
				continue
			}
			seq := gjson.Get(payload, "sequence_number")
			require.True(t, seq.Exists(), payload)
			require.Equal(t, previous+1, seq.Int(), payload)
			previous = seq.Int()
			failed = failed || gjson.Get(payload, "type").String() == "response.failed"
		}
		require.True(t, failed)
	}
}

func TestCNReviewBufferedLineWinsOverExpiredIdleTimer(t *testing.T) {
	for i := 0; i < 50; i++ {
		pump := &anthropicNativeLinePump{events: make(chan anthropicNativeLineEvent, 1), done: make(chan struct{}), timer: time.NewTimer(0), interval: time.Second}
		pump.events <- anthropicNativeLineEvent{line: "data: ready"}
		line, err := pump.next()
		pump.stop()
		require.NoError(t, err, "buffered data must not be classified as idle")
		require.Equal(t, "data: ready", line)
	}
}
