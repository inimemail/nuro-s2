package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCNReviewNativeResponsesKeepsReasoningPolicy(t *testing.T) {
	for _, signature := range []string{"", "gAAAA-codex-signature", "xai-cipher"} {
		t.Run(signature, func(t *testing.T) {
			blocks, err := json.Marshal([]AnthropicContentBlock{
				{Type: "thinking", Thinking: "inspect workspace", Signature: signature},
				{Type: "tool_use", ID: "toolu_a", Name: "exec", Input: json.RawMessage(`{"cmd":"pwd"}`)},
			})
			require.NoError(t, err)
			req := &AnthropicRequest{Messages: []AnthropicMessage{
				{Role: "assistant", Content: blocks},
				{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_a","content":"ok"}]`)},
			}}
			native, err := AnthropicToResponses(req)
			require.NoError(t, err)
			var nativeItems []ResponsesInputItem
			require.NoError(t, json.Unmarshal(native.Input, &nativeItems))
			if signature == "xai-cipher" {
				require.Len(t, nativeItems, 3)
				require.Equal(t, signature, nativeItems[0].EncryptedContent)
				require.Empty(t, nativeItems[0].Summary)
			} else {
				require.Len(t, nativeItems, 2)
				require.Equal(t, "function_call", nativeItems[0].Type)
			}
			bridge, err := AnthropicToResponsesForChatCompletions(req)
			require.NoError(t, err)
			chat, err := ResponsesToChatCompletionsRequest(bridge)
			require.NoError(t, err)
			require.Len(t, chat.Messages, 2)
			require.Equal(t, "inspect workspace", chat.Messages[0].ReasoningContent)
			require.Len(t, chat.Messages[0].ToolCalls, 1)
		})
	}
}

func TestCNReviewNativeInitialContentAndEmptyToolDelta(t *testing.T) {
	for _, block := range []AnthropicContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "thinking", Thinking: "inspect"},
		{Type: "tool_use", ID: "c1", Name: "exec", Input: json.RawMessage(`{"cmd":"pwd"}`)},
	} {
		t.Run(block.Type, func(t *testing.T) {
			state := NewAnthropicEventToResponsesState()
			events := []AnthropicStreamEvent{
				{Type: "message_start", Message: &AnthropicResponse{ID: "m1"}},
				{Type: "content_block_start", ContentBlock: &block},
			}
			if block.Type == "tool_use" {
				events = append(events, AnthropicStreamEvent{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "input_json_delta"}})
			}
			events = append(events, AnthropicStreamEvent{Type: "content_block_stop"}, AnthropicStreamEvent{Type: "message_stop"})
			var terminal *ResponsesResponse
			for _, event := range events {
				for _, out := range AnthropicEventToResponsesEvents(&event, state) {
					if out.Type == "response.completed" {
						terminal = out.Response
					}
				}
			}
			require.NotNil(t, terminal)
			require.Len(t, terminal.Output, 1)
			switch block.Type {
			case "text":
				require.Len(t, terminal.Output[0].Content, 1)
				require.Equal(t, "hello", terminal.Output[0].Content[0].Text)
			case "thinking":
				require.Len(t, terminal.Output[0].Summary, 1)
				require.Equal(t, "inspect", terminal.Output[0].Summary[0].Text)
			case "tool_use":
				require.JSONEq(t, `{"cmd":"pwd"}`, terminal.Output[0].Arguments)
			}
		})
	}
}

func TestCNReviewToolSearchTextInputParity(t *testing.T) {
	call := ChatToolCall{ID: "c1", Type: "function", Function: ChatFunctionCall{Name: "tool_search", Arguments: "find workspace tools"}}
	resp := &ChatCompletionsResponse{Choices: []ChatChoice{{Message: ChatMessage{ToolCalls: []ChatToolCall{call}}, FinishReason: "tool_calls"}}}
	out := ChatCompletionsResponseToResponses(resp, "test", nil, true, nil)
	require.Equal(t, "completed", out.Status)
	require.Len(t, out.Output, 1)
	require.Equal(t, "tool_search_call", out.Output[0].Type)
	require.Equal(t, call.Function.Arguments, out.Output[0].Arguments)
	state := NewChatCompletionsToResponsesStreamState("test")
	state.ToolSearchDeclared = true
	ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{call}}}}}, state)
	require.NoError(t, state.ValidateToolCallArguments())
	events := FinalizeChatCompletionsResponsesStream(state)
	require.Equal(t, out.Output[0].Arguments, events[len(events)-1].Response.Output[0].Arguments)
	// A regular function named tool_search still requires JSON arguments.
	require.Error(t, ValidateChatToolCalls(resp, nil, false, nil))
}
