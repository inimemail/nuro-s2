package apicompat

import (
	"encoding/json"
	"testing"
)

func TestCNBridgeMessagesThinkingReplay(t *testing.T) {
	req := &AnthropicRequest{Model: "deepseek-reasoner", Messages: []AnthropicMessage{
		{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"inspect workspace"},{"type":"tool_use","id":"toolu_a","name":"exec","input":{"cmd":"pwd"}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_a","content":"/workspace"}]`)},
	}}
	res, err := AnthropicToResponsesForChatCompletions(req)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := ResponsesToChatCompletionsRequest(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range chat.Messages {
		if len(msg.ToolCalls) > 0 && msg.ReasoningContent != "inspect workspace" {
			t.Errorf("Messages thinking lost: %q", msg.ReasoningContent)
		}
	}
}

func TestCNBridgeCreatedAt(t *testing.T) {
	responses := map[string]*ResponsesResponse{
		"chat":      ChatCompletionsResponseToResponses(&ChatCompletionsResponse{Choices: []ChatChoice{{Message: ChatMessage{Content: json.RawMessage(`"hi"`)}, FinishReason: "stop"}}}, "test", nil, false, nil),
		"anthropic": AnthropicToResponsesResponse(&AnthropicResponse{ID: "msg_1", Content: []AnthropicContentBlock{{Type: "text", Text: "hi"}}, StopReason: AnthropicStopReasonPtr("end_turn")}),
	}
	for name, res := range responses {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			json.Unmarshal(b, &wire)
			if _, ok := wire["created_at"]; !ok {
				t.Errorf("required created_at absent: %s", b)
			}
		})
	}
}

func TestCNBridgeReasoningAlias(t *testing.T) {
	var resp ChatCompletionsResponse
	if err := json.Unmarshal([]byte(`{"choices":[{"message":{"role":"assistant","reasoning":"inspect workspace","tool_calls":[{"id":"c1","type":"function","function":{"name":"exec","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`), &resp); err != nil {
		t.Fatal(err)
	}
	out := ChatCompletionsResponseToResponses(&resp, "test", nil, false, nil)
	for _, item := range out.Output {
		if item.Type == "reasoning" {
			return
		}
	}
	t.Error("compatible reasoning field dropped")
}

func TestCNBridgeInvalidToolJSON(t *testing.T) {
	out := ChatCompletionsResponseToResponses(&ChatCompletionsResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "c1", Function: ChatFunctionCall{Name: "exec", Arguments: `{"cmd":`}}}}, FinishReason: "tool_calls"}}}, "test", nil, false, nil)
	if out.Status == "completed" {
		t.Errorf("invalid tool arguments silently accepted as completed: %+v", out.Output)
	}
}

func TestCNBridgeParallelDisabled(t *testing.T) {
	disabled := false
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{Input: json.RawMessage(`"hi"`), ParallelToolCalls: &disabled, Tools: []ResponsesTool{{Type: "function", Name: "exec", Parameters: json.RawMessage(`{"type":"object"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	var choice map[string]any
	json.Unmarshal(out.ToolChoice, &choice)
	if choice["disable_parallel_tool_use"] != true {
		t.Errorf("parallel_tool_calls=false lost: %s", out.ToolChoice)
	}
}

func TestCNBridgeThinkingBudget(t *testing.T) {
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{Input: json.RawMessage(`"hi"`), Reasoning: &ResponsesReasoning{Effort: "high"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking != nil && out.Thinking.BudgetTokens >= out.MaxTokens {
		t.Errorf("thinking budget %d exceeds max_tokens %d", out.Thinking.BudgetTokens, out.MaxTokens)
	}
}

func TestCNBridgeInterleavedThinkingKeepsText(t *testing.T) {
	state := NewAnthropicEventToResponsesState()
	events := []AnthropicStreamEvent{
		{Type: "message_start", Message: &AnthropicResponse{ID: "m1"}},
		{Type: "content_block_start", ContentBlock: &AnthropicContentBlock{Type: "text"}},
		{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "text_delta", Text: "starting inspection"}},
		{Type: "content_block_stop"},
		{Type: "content_block_start", ContentBlock: &AnthropicContentBlock{Type: "thinking"}},
		{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "thinking_delta", Thinking: "next step"}},
		{Type: "content_block_stop"},
		{Type: "message_delta", Delta: &AnthropicDelta{StopReason: "end_turn"}},
		{Type: "message_stop"},
	}
	for _, evt := range events {
		for _, out := range AnthropicEventToResponsesEvents(&evt, state) {
			if out.Response != nil && out.Response.Status == "completed" {
				for _, item := range out.Response.Output {
					for _, part := range item.Content {
						if part.Text == "starting inspection" {
							return
						}
					}
				}
			}
		}
	}
	t.Error("text before thinking disappeared from final output")
}

func TestCNBridgeToolArgumentsDoneRecovery(t *testing.T) {
	state := NewResponsesEventToChatState()
	events := []ResponsesStreamEvent{
		{Type: "response.output_item.added", OutputIndex: 0, Item: &ResponsesOutput{Type: "function_call", CallID: "c1", Name: "exec"}},
		{Type: "response.function_call_arguments.delta", OutputIndex: 0, Delta: `{"cmd":`},
		{Type: "response.function_call_arguments.done", OutputIndex: 0, Arguments: `{"cmd":"pwd"}`},
		{Type: "response.completed", Response: &ResponsesResponse{Status: "completed"}},
	}
	args := ""
	for _, evt := range events {
		for _, chunk := range ResponsesEventToChatChunks(&evt, state) {
			for _, choice := range chunk.Choices {
				for _, tool := range choice.Delta.ToolCalls {
					args += tool.Function.Arguments
				}
			}
		}
	}
	if args != `{"cmd":"pwd"}` {
		t.Errorf("final complete tool arguments ignored, downstream got %q", args)
	}
}
