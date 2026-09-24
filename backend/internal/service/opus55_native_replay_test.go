//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV028NativeAnthropicOpusReplayIsAccountBound(t *testing.T) {
	const events = `data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5-5","role":"assistant","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"provider-signature"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}

data: {"type":"content_block_stop","index":1}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

data: {"type":"message_stop"}

`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			a := rawChatCompletionsTestAccount()
			a.Platform = PlatformOpenCodeGo
			a.Credentials["api_protocol"] = APIProtocolAnthropic
			a.Credentials["model_mapping"] = map[string]any{"alias": "claude-opus-5-5"}
			forward := func(input any) (*httptest.ResponseRecorder, error) {
				upstream.resp = &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(events))}
				body, err := json.Marshal(map[string]any{"model": "alias", "stream": stream, "input": input, "temperature": 0.5})
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
				_, err = svc.forwardResponsesViaNativeAnthropic(context.Background(), c, a, body, "")
				return rec, err
			}
			rec, err := forward("hello")
			require.NoError(t, err)
			require.Equal(t, "adaptive", gjson.GetBytes(upstream.lastBody, "thinking.type").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "temperature").Exists())
			response := gjson.ParseBytes(rec.Body.Bytes())
			if stream {
				for _, line := range strings.Split(rec.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					event := gjson.Parse(strings.TrimPrefix(line, "data: "))
					if event.Get("type").String() == "response.completed" {
						response = event.Get("response")
					}
				}
			}
			require.Equal(t, "alias", response.Get("model").String())
			require.NotEmpty(t, response.Get("output.0.encrypted_content").String())
			input := json.RawMessage(response.Get("output").Raw)
			_, err = forward(input)
			require.NoError(t, err)
			require.Equal(t, "provider-signature", gjson.GetBytes(upstream.lastBody, "messages.0.content.0.signature").String())
			calls := len(upstream.requests)
			a.ID++
			_, err = forward(input)
			require.ErrorContains(t, err, "thinking envelope")
			require.Len(t, upstream.requests, calls, "cross-account replay must fail before sending upstream")
		})
	}
}
