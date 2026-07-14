package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestV152ResponsesToolsBridgeAndChoice(t *testing.T) {
	req := &ResponsesRequest{
		Model: "chat-only",
		Input: json.RawMessage(`"use tools"`),
		Tools: []ResponsesTool{
			{Type: "custom", Name: "exec"},
			{Type: "tool_search"},
			{Type: "namespace", Name: "gmail", Tools: []ResponsesTool{{Type: "function", Name: "send"}}},
		},
		ToolChoice: json.RawMessage(`{"type":"tool_search"}`),
	}

	out, err := ResponsesToChatCompletionsRequest(req)
	require.NoError(t, err)
	require.Len(t, out.Tools, 3)
	assert.Equal(t, "exec", out.Tools[0].Function.Name)
	assert.JSONEq(t, customToolInputSchema, string(out.Tools[0].Function.Parameters))
	assert.Equal(t, "tool_search", out.Tools[1].Function.Name)
	assert.Equal(t, "gmail__send", out.Tools[2].Function.Name)
	assert.JSONEq(t, `{"type":"function","function":{"name":"tool_search"}}`, string(out.ToolChoice))
}

func TestV152ResponsesToolsRejectAmbiguousNames(t *testing.T) {
	_, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model: "chat-only",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{
			{Type: "tool_search"},
			{Type: "function", Name: "tool_search"},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool_search")

	_, err = ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model: "chat-only",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{
			{Type: "function", Name: "gmail__send"},
			{Type: "namespace", Name: "gmail", Tools: []ResponsesTool{{Type: "function", Name: "send"}}},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gmail__send")
}

func TestV152NamespaceToolChoiceUsesFlattenedName(t *testing.T) {
	out, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model:      "chat-only",
		Input:      json.RawMessage(`"hi"`),
		Tools:      []ResponsesTool{{Type: "namespace", Name: "gmail", Tools: []ResponsesTool{{Type: "function", Name: "send"}}}},
		ToolChoice: json.RawMessage(`{"type":"function","namespace":"gmail","name":"send"}`),
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"function","function":{"name":"gmail__send"}}`, string(out.ToolChoice))
}

func TestV152CompletedOutputReusesStreamItemIDs(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("chat-only")
	state.ReasoningItemID = "reasoning_item_1"
	_, _ = state.Reasoning.WriteString("think")
	state.ToolCalls[0] = &ChatToolCall{ID: "call_1", Function: ChatFunctionCall{Name: "exec", Arguments: `{"input":"pwd"}`}}
	state.ToolItemIDs[0] = "tool_item_1"
	state.toolIsCustom[0] = true

	output := state.chatOutput()
	require.Len(t, output, 2)
	assert.Equal(t, "reasoning_item_1", output[0].ID)
	assert.Equal(t, "tool_item_1", output[1].ID)
}

func TestV152IncompleteTerminalReusesAllStreamItemIDs(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("chat-only")
	state.ReasoningItemID = "reasoning_item_1"
	state.MessageItemID = "message_item_1"
	state.FinishReason = "length"
	_, _ = state.Reasoning.WriteString("think")
	_, _ = state.Text.WriteString("partial")
	state.ToolCalls[0] = &ChatToolCall{ID: "call_1", Function: ChatFunctionCall{Name: "exec", Arguments: `{}`}}
	state.ToolItemIDs[0] = "tool_item_1"

	events := FinalizeChatCompletionsResponsesStream(state)
	require.NotEmpty(t, events)
	terminal := events[len(events)-1]
	require.Equal(t, "response.incomplete", terminal.Type)
	require.NotNil(t, terminal.Response)
	require.Equal(t, "incomplete", terminal.Response.Status)
	require.NotNil(t, terminal.Response.IncompleteDetails)
	require.Equal(t, "max_output_tokens", terminal.Response.IncompleteDetails.Reason)
	require.Len(t, terminal.Response.Output, 3)
	assert.Equal(t, "reasoning_item_1", terminal.Response.Output[0].ID)
	assert.Equal(t, "message_item_1", terminal.Response.Output[1].ID)
	assert.Equal(t, "tool_item_1", terminal.Response.Output[2].ID)
	for _, item := range terminal.Response.Output {
		if item.Status != "" {
			assert.Equal(t, "incomplete", item.Status)
		}
	}
}

func TestV152SparseToolCallIndexIsFinalized(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("chat-only")
	index := 2
	name := "lookup"
	arguments := `{"query":"status"}`

	events := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{{
			Index: &index,
			ID:    "call_sparse",
			Type:  "function",
			Function: ChatFunctionCall{
				Name:      name,
				Arguments: arguments,
			},
		}}}}},
	}, state)
	require.NotEmpty(t, events)

	final := FinalizeChatCompletionsResponsesStream(state)
	require.NotEmpty(t, final)
	terminal := final[len(final)-1]
	require.Equal(t, "response.completed", terminal.Type)
	require.NotNil(t, terminal.Response)
	require.Len(t, terminal.Response.Output, 1)
	require.Equal(t, "call_sparse", terminal.Response.Output[0].CallID)
	require.Equal(t, "lookup", terminal.Response.Output[0].Name)
	require.JSONEq(t, arguments, terminal.Response.Output[0].Arguments)

	var sawDone bool
	for _, event := range final {
		if event.Type == "response.output_item.done" && event.Item != nil && event.Item.CallID == "call_sparse" {
			sawDone = true
		}
	}
	require.True(t, sawDone)
}

func TestV152ChatToolCallsRestoreResponsesTypes(t *testing.T) {
	resp := &ChatCompletionsResponse{Choices: []ChatChoice{{Message: ChatMessage{
		Role: "assistant",
		ToolCalls: []ChatToolCall{
			{ID: "call_exec", Function: ChatFunctionCall{Name: "exec", Arguments: `{"input":"pwd"}`}},
			{ID: "call_search", Function: ChatFunctionCall{Name: "tool_search", Arguments: `{"query":"gmail","limit":2}`}},
			{ID: "call_send", Function: ChatFunctionCall{Name: "gmail__send", Arguments: `{"to":"a@example.com"}`}},
		},
	}}}}

	out := ChatCompletionsResponseToResponses(
		resp,
		"chat-only",
		map[string]bool{"exec": true},
		true,
		map[string]NamespacedToolName{"gmail__send": {Namespace: "gmail", Name: "send"}},
	)
	require.Len(t, out.Output, 3)
	assert.Equal(t, "custom_tool_call", out.Output[0].Type)
	assert.Equal(t, "pwd", out.Output[0].Input)
	assert.Equal(t, "tool_search_call", out.Output[1].Type)
	assert.Equal(t, "function_call", out.Output[2].Type)
	assert.Equal(t, "gmail", out.Output[2].Namespace)
	assert.Equal(t, "send", out.Output[2].Name)

	wire, err := json.Marshal(out.Output[1])
	require.NoError(t, err)
	var item map[string]any
	require.NoError(t, json.Unmarshal(wire, &item))
	assert.Equal(t, "client", item["execution"])
	args, ok := item["arguments"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "gmail", args["query"])
}

func TestV152ToolSearchObjectArgumentsRoundTrip(t *testing.T) {
	var item ResponsesOutput
	require.NoError(t, json.Unmarshal([]byte(`{
		"type":"tool_search_call",
		"id":"item_1",
		"call_id":"call_1",
		"execution":"client",
		"arguments":{"query":"gmail","limit":2}
	}`), &item))
	assert.JSONEq(t, `{"query":"gmail","limit":2}`, item.Arguments)

	wire, err := json.Marshal(item)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(wire, &decoded))
	_, ok := decoded["arguments"].(map[string]any)
	assert.True(t, ok)
}

func TestV152StreamWireKeepsZeroIndexes(t *testing.T) {
	wire, err := json.Marshal(ResponsesStreamEvent{
		Type: "response.custom_tool_call_input.done", OutputIndex: 0,
		ItemID: "ct_1", CallID: "call_1", Name: "exec", Input: "pwd",
	})
	require.NoError(t, err)
	var event map[string]any
	require.NoError(t, json.Unmarshal(wire, &event))
	assert.Contains(t, event, "output_index")
	assert.EqualValues(t, 0, event["output_index"])
	assert.Equal(t, "pwd", event["input"])
}

func TestV152StreamDefersToolAnnouncementUntilIdentityIsStable(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("chat-only")
	index := 0

	first := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{{
			Index: &index,
			ID:    "call_1",
			Function: ChatFunctionCall{
				Arguments: `{"query":"sta`,
			},
		}}}}},
	}, state)
	for _, event := range first {
		require.NotEqual(t, "response.output_item.added", event.Type)
	}

	second := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{{
			Index: &index,
			Function: ChatFunctionCall{
				Name:      "lookup",
				Arguments: `tus"}`,
			},
		}}}}},
	}, state)
	var added *ResponsesOutput
	for i := range second {
		if second[i].Type == "response.output_item.added" {
			added = second[i].Item
		}
	}
	require.NotNil(t, added)
	require.Equal(t, "call_1", added.CallID)
	require.Equal(t, "lookup", added.Name)

	final := FinalizeChatCompletionsResponsesStream(state)
	terminal := final[len(final)-1]
	require.Equal(t, "call_1", terminal.Response.Output[0].CallID)
	require.Equal(t, "lookup", terminal.Response.Output[0].Name)
	require.JSONEq(t, `{"query":"status"}`, terminal.Response.Output[0].Arguments)
}

func TestV152StreamDefersToolAnnouncementUntilRealCallIDArrives(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("chat-only")
	index := 0

	first := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{{
			Index:    &index,
			Function: ChatFunctionCall{Name: "lookup", Arguments: `{}`},
		}}}}},
	}, state)
	for _, event := range first {
		require.NotEqual(t, "response.output_item.added", event.Type)
	}

	second := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{{
			Index: &index,
			ID:    "call_late",
		}}}}},
	}, state)
	var added *ResponsesOutput
	for i := range second {
		if second[i].Type == "response.output_item.added" {
			added = second[i].Item
		}
	}
	require.NotNil(t, added)
	require.Equal(t, "call_late", added.CallID)
	require.Equal(t, "lookup", added.Name)
}

func TestV152StreamResponseIDDoesNotChangeAfterCreated(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("chat-only")
	first := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{}, state)
	require.NotEmpty(t, first)
	createdID := first[0].Response.ID
	require.NotEmpty(t, createdID)

	ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{ID: "late-upstream-id"}, state)
	final := FinalizeChatCompletionsResponsesStream(state)
	require.Equal(t, createdID, final[len(final)-1].Response.ID)
}
