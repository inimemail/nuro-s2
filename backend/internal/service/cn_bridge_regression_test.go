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
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const cnBroadStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"test\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"

func TestCNBridgeNativeFailureNotSuccess(t *testing.T) {
	for _, kind := range []string{"error", "truncated", "early_error", "invalid_json", "orphan_delta", "invalid_tool"} {
		for _, mode := range []string{"responses_buffer", "responses_stream", "chat_buffer", "chat_stream"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				setGinTestMode()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				body := cnBroadStart
				if kind == "error" {
					body += "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"
				}
				if kind == "early_error" {
					body = "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"
				}
				if kind == "invalid_json" {
					body += "data: {\n\n"
				}
				if kind == "orphan_delta" {
					body += "data: {\"type\":\"content_block_delta\",\"index\":7,\"delta\":{\"type\":\"text_delta\",\"text\":\"bad\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				if kind == "invalid_tool" {
					body += "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c1\",\"name\":\"exec\",\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"cmd\\\":\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
				var err error
				switch mode {
				case "responses_buffer":
					_, err = svc.handleResponsesBufferedFromNativeAnthropic(resp, c, "test", "test", "test", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
				case "responses_stream":
					_, err = svc.handleResponsesStreamingFromNativeAnthropic(resp, c, "test", "test", "test", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
				case "chat_buffer":
					_, err = svc.handleCCBufferedFromNativeAnthropic(resp, c, "test", "test", "test", nil, time.Now())
				case "chat_stream":
					_, err = svc.handleCCStreamingFromNativeAnthropic(resp, c, "test", "test", "test", nil, time.Now(), false)
				}
				if err == nil {
					t.Errorf("%s accepted as success: status=%d body=%s", kind, rec.Code, rec.Body.String())
				}
				require.NotContains(t, rec.Body.String(), "response.completed")
				require.NotContains(t, rec.Body.String(), `"finish_reason":"stop"`)
				require.Contains(t, rec.Body.String(), "error")
				if strings.HasSuffix(mode, "buffer") || kind == "early_error" {
					require.Equal(t, http.StatusBadGateway, rec.Code)
				}
			})
		}
	}
}

func TestCNBridgeBufferedNativeToolArguments(t *testing.T) {
	setGinTestMode()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	body := cnBroadStart + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"exec","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"pwd\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	_, err := svc.handleResponsesBufferedFromNativeAnthropic(&http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, c, "test", "test", "test", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
	if err != nil {
		t.Fatal(err)
	}
	args := gjson.Get(rec.Body.String(), "output.0.arguments").String()
	if args != `{"cmd":"pwd"}` {
		t.Errorf("tool args corrupted: %q", args)
	}
}

func TestCNBridgeGLMResponsesEffortNormalization(t *testing.T) {
	setGinTestMode()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"c1","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`))}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	acc := rawChatCompletionsTestAccount()
	acc.Platform = PlatformZhipu
	_, err := svc.Forward(context.Background(), c, acc, []byte(`{"model":"glm-5","input":"hi","reasoning":{"effort":"xhigh"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(upstream.lastBody, "reasoning_effort").String(); got != "max" {
		t.Errorf("GLM received unnormalized effort %q", got)
	}
}

func TestCNBridgeChatIdleTimeout(t *testing.T) {
	for _, mode := range []string{"raw_chat", "messages_bridge"} {
		t.Run(mode, func(t *testing.T) {
			setGinTestMode()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			cfg := rawChatCompletionsTestConfig()
			cfg.Gateway.StreamDataIntervalTimeout = 1
			svc := &OpenAIGatewayService{cfg: cfg}
			acc := rawChatCompletionsTestAccount()
			acc.Platform = PlatformKimi
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}
			done := make(chan error, 1)
			go func() {
				var err error
				if mode == "raw_chat" {
					_, err = svc.streamRawChatCompletions(context.Background(), c, resp, acc, "test", "test", "test", nil, nil, time.Now(), 0, openAIRequestFirstTokenPlaceholderState{})
				} else {
					_, err = svc.streamChatCompletionsAsAnthropic(context.Background(), c, resp, acc, "test", "test", "test", nil, nil, nil, false, nil, time.Now())
				}
				done <- err
			}()
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(1500 * time.Millisecond):
				t.Error("stream still blocked past configured 1-second idle timeout")
				writer.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("stream did not exit after EOF")
				}
			}
		})
	}
}

func TestCNBridgeProviderRoutingMatrix(t *testing.T) {
	for _, platform := range []string{PlatformDeepSeek, PlatformKimi, PlatformZhipu, PlatformMiniMax} {
		for _, entry := range []string{"responses", "messages"} {
			t.Run(platform+"/"+entry, func(t *testing.T) {
				setGinTestMode()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/"+entry, nil)
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"c1","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`))}}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				acc := rawChatCompletionsTestAccount()
				acc.Platform = platform
				acc.Extra = map[string]any{"cn_api_mode": "chat_completions"}
				if entry == "responses" {
					_, err := svc.Forward(context.Background(), c, acc, []byte(`{"model":"test","tools":[{"type":"tool_search"}],"input":[{"type":"tool_search_call","call_id":"s1","arguments":{"query":"workspace"}},{"type":"tool_search_output","call_id":"s1","status":"completed","execution":"client","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}]}`))
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, tool := range gjson.GetBytes(upstream.lastBody, "tools").Array() {
						if tool.Get("function.name").String() == "exec" {
							found = true
						}
					}
					if !found {
						t.Error("discovered exec lost through provider routing")
					}
				} else {
					_, err := svc.ForwardAsAnthropic(context.Background(), c, acc, []byte(`{"model":"test","max_tokens":4096,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"inspect workspace"},{"type":"tool_use","id":"toolu_a","name":"exec","input":{"cmd":"pwd"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"/workspace"}]}]}`), "", "")
					if err != nil {
						t.Fatal(err)
					}
					toolCount := 0
					for _, msg := range gjson.GetBytes(upstream.lastBody, "messages").Array() {
						toolCount += len(msg.Get("tool_calls").Array())
						if len(msg.Get("tool_calls").Array()) > 0 && msg.Get("reasoning_content").String() != "inspect workspace" {
							t.Error("thinking lost through provider routing")
						}
					}
					require.Equal(t, 1, toolCount)
				}
			})
		}
	}
}

// Exercise both stream converters and both buffered converters with the same
// upstream turn, including providers which put complete input in block_start.
func TestCNBridgeNativeToolSuccessAndUsage(t *testing.T) {
	for _, initial := range []bool{false, true} {
		for _, mode := range []string{"responses_buffer", "responses_stream", "chat_buffer", "chat_stream"} {
			t.Run(fmt.Sprintf("initial=%t/%s", initial, mode), func(t *testing.T) {
				setGinTestMode()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				input := `{}`
				if initial {
					input = `{"cmd":"pwd"}`
				}
				body := cnBroadStart + "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"exec\",\"input\":" + input + "}}\n\n"
				if !initial {
					body += "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"cmd\\\":\\\"pwd\\\"}\"}}\n\n"
				}
				body += "data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
				resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
				var result *OpenAIForwardResult
				var err error
				switch mode {
				case "responses_buffer":
					result, err = svc.handleResponsesBufferedFromNativeAnthropic(resp, c, "test", "test", "test", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
				case "responses_stream":
					result, err = svc.handleResponsesStreamingFromNativeAnthropic(resp, c, "test", "test", "test", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
				case "chat_buffer":
					result, err = svc.handleCCBufferedFromNativeAnthropic(resp, c, "test", "test", "test", nil, time.Now())
				case "chat_stream":
					result, err = svc.handleCCStreamingFromNativeAnthropic(resp, c, "test", "test", "test", nil, time.Now(), true)
				}
				require.NoError(t, err)
				require.Equal(t, 200, rec.Code)
				require.EqualValues(t, 1, result.Usage.InputTokens)
				require.EqualValues(t, 5, result.Usage.OutputTokens)
				require.NotContains(t, rec.Body.String(), "response.failed")
				args := ""
				switch mode {
				case "responses_buffer":
					args = gjson.Get(rec.Body.String(), "output.0.arguments").String()
				case "chat_buffer":
					args = gjson.Get(rec.Body.String(), "choices.0.message.tool_calls.0.function.arguments").String()
				default:
					for _, line := range strings.Split(rec.Body.String(), "\n") {
						payload, ok := extractOpenAISSEDataLine(line)
						if !ok || !json.Valid([]byte(payload)) {
							continue
						}
						if mode == "responses_stream" {
							if gjson.Get(payload, "type").String() == "response.function_call_arguments.delta" {
								args += gjson.Get(payload, "delta").String()
							}
						} else {
							args += gjson.Get(payload, "choices.0.delta.tool_calls.0.function.arguments").String()
						}
					}
				}
				require.JSONEq(t, `{"cmd":"pwd"}`, args)
			})
		}
	}
}
