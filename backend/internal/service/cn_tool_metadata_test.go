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

func TestCNToolMetadataAndCustomCallRouting(t *testing.T) {
	for _, platform := range []string{PlatformDeepSeek, PlatformKimi, PlatformZhipu, PlatformMiniMax} {
		for _, protocol := range []string{APIProtocolChatCompletions, APIProtocolAnthropic} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", platform, protocol, stream), func(t *testing.T) {
					setGinTestMode()
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
					const rawInput = `const result = await tools.exec_command({cmd: "pwd"}); text(result);`
					arguments, err := json.Marshal(map[string]string{"input": rawInput})
					require.NoError(t, err)
					call := map[string]any{"id": "call_1", "type": "function", "index": 0, "function": map[string]any{"name": "exec", "arguments": string(arguments)}}
					response, err := json.Marshal(map[string]any{"id": "chat_1", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": []any{call}}, "finish_reason": "tool_calls"}}})
					require.NoError(t, err)
					if stream {
						chunk, err := json.Marshal(map[string]any{"id": "chat_1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}, "finish_reason": "tool_calls"}}})
						require.NoError(t, err)
						response = []byte("data: " + string(chunk) + "\n\ndata: [DONE]\n\n")
					}
					if protocol == APIProtocolAnthropic {
						block, err := json.Marshal(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call_1", "name": "exec", "input": json.RawMessage(arguments)}})
						require.NoError(t, err)
						response = []byte(cnBroadStart + "data: " + string(block) + "\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(response)))}}
					svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
					account := rawChatCompletionsTestAccount()
					account.Platform = platform
					account.Extra = map[string]any{"cn_api_mode": protocol}
					body := fmt.Sprintf(`{"model":"test","stream":%t,"input":"inspect workspace","tools":[{"type":"custom","name":"exec","description":"Run JavaScript.","format":{"type":"grammar","syntax":"lark","definition":"start: /[\\s\\S]+/"}},{"type":"namespace","name":"functions","description":"Workspace tool instructions.","tools":[{"type":"function","name":"wait","description":"Wait for a command.","parameters":{"type":"object"}}]}]}`, stream)
					_, err = svc.Forward(context.Background(), c, account, []byte(body))
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, rec.Code)
					tools := gjson.GetBytes(upstream.lastBody, "tools").Array()
					require.Len(t, tools, 2)
					prefix := ""
					if protocol == APIProtocolChatCompletions {
						prefix = "function."
					}
					require.Contains(t, tools[0].Get(prefix+"description").String(), "start:")
					require.Contains(t, tools[0].Get(prefix+"description").String(), `"input"`)
					require.Contains(t, tools[1].Get(prefix+"description").String(), "Workspace tool instructions.")
					var terminal gjson.Result
					if stream {
						for _, line := range strings.Split(rec.Body.String(), "\n") {
							if strings.HasPrefix(line, "data: ") {
								event := gjson.Parse(strings.TrimPrefix(line, "data: "))
								if event.Get("type").String() == "response.completed" {
									terminal = event.Get("response")
								}
							}
						}
					} else {
						terminal = gjson.Parse(rec.Body.String())
					}
					require.Equal(t, "completed", terminal.Get("status").String())
					require.Equal(t, "custom_tool_call", terminal.Get("output.0.type").String())
					require.Equal(t, "exec", terminal.Get("output.0.name").String())
					require.Equal(t, rawInput, terminal.Get("output.0.input").String())
				})
			}
		}
	}
}
