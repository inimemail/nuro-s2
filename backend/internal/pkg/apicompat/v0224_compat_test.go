package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBufferedResponsesPreserveSlotsAndContentParts(t *testing.T) {
	acc := NewBufferedResponseAccumulator()
	for _, event := range []ResponsesStreamEvent{
		{Type: "response.output_text.delta", OutputIndex: 0, ContentIndex: 1, Delta: "second"},
		{Type: "response.output_text.delta", OutputIndex: 0, ContentIndex: 0, Delta: "first"},
		{Type: "response.output_text.done", OutputIndex: 0, ContentIndex: 0, Text: "final first"},
		{Type: "response.output_item.done", OutputIndex: 1, Item: &ResponsesOutput{Type: "web_search_call", ID: "ws_fixture", Status: "completed"}},
		{Type: "response.output_item.done", OutputIndex: 2, Item: &ResponsesOutput{Type: "web_search_call", ID: "ws_fixture", Status: "completed"}},
		{Type: "response.output_text.delta", OutputIndex: 3, Delta: "tail"},
	} {
		acc.ProcessEvent(&event)
	}
	resp := &ResponsesResponse{Status: "completed", Usage: &ResponsesUsage{InputTokens: 10}}
	acc.SupplementResponseOutput(resp)
	require.Len(t, resp.Output, 3)
	require.Equal(t, "final first", resp.Output[0].Content[0].Text)
	require.Equal(t, "second", resp.Output[0].Content[1].Text)
	require.Equal(t, "ws_fixture", resp.Output[1].ID)
	require.Equal(t, "tail", resp.Output[2].Content[0].Text)
	require.Equal(t, 10, resp.Usage.InputTokens)
}

func TestBufferedResponsesPreserveCustomToolIdentity(t *testing.T) {
	acc := NewBufferedResponseAccumulator()
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.added", Item: &ResponsesOutput{
		ID: "ct_fixture", Type: "custom_tool_call", CallID: "call_fixture", Name: "format",
	}})
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.custom_tool_call_input.delta", Delta: `{"style":"plain"}`})
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.done", Item: &ResponsesOutput{Type: "custom_tool_call"}})

	resp := &ResponsesResponse{Status: "completed"}
	acc.SupplementResponseOutput(resp)
	require.Len(t, resp.Output, 1)
	require.Equal(t, "ct_fixture", resp.Output[0].ID)
	require.Equal(t, "custom_tool_call", resp.Output[0].Type)
	require.Equal(t, "call_fixture", resp.Output[0].CallID)
	require.Equal(t, `{"style":"plain"}`, resp.Output[0].Input)
	require.Equal(t, resp.Output[0].Input, ResponsesToChatCompletions(resp, "public").Choices[0].Message.ToolCalls[0].Function.Arguments)
	require.JSONEq(t, resp.Output[0].Input, string(ResponsesToAnthropic(resp, "public").Content[0].Input))
}

func TestNativeAnthropicToolSearchPreservesDeferredDefinitions(t *testing.T) {
	var request AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"gpt-6.2-sol",
		"tools":[{"name":"ToolSearch","input_schema":{"type":"object"}},{"name":"Lookup","defer_loading":true}],
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"ToolSearch","input":{"query":"Lookup"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":[{"type":"tool_reference","tool_name":"Lookup"},{"type":"text","text":"Found it"}]}]}
		]
	}`), &request))
	out, err := AnthropicToResponses(&request)
	require.NoError(t, err)
	require.NoError(t, ApplyAnthropicNativeToolSearch(&request, out))
	require.Len(t, out.Tools, 1)
	require.Equal(t, "tool_search", out.Tools[0].Type)
	require.Equal(t, "client", out.Tools[0].Execution)
	input := gjson.ParseBytes(out.Input)
	require.Equal(t, "tool_search_call", input.Get("0.type").String())
	require.Equal(t, "tool_search_output", input.Get("1.type").String())
	require.Equal(t, "Lookup", input.Get("1.tools.0.name").String())
	require.True(t, input.Get("1.tools.0.defer_loading").Bool())
	require.Contains(t, input.Get("2.content.0.text").String(), "Found it")
	var result ResponsesOutput
	require.NoError(t, json.Unmarshal([]byte(`{"type":"tool_search_call","execution":"client","call_id":"call_native","status":"completed","arguments":{"query":"Lookup"}}`), &result))
	message := ResponsesToAnthropic(&ResponsesResponse{Output: []ResponsesOutput{result}}, "public")
	require.Equal(t, "ToolSearch", message.Content[0].Name)
	require.JSONEq(t, `{"query":"Lookup"}`, string(message.Content[0].Input))
}

func TestResponsesRefusalDonePreservesFinalText(t *testing.T) {
	var event ResponsesStreamEvent
	require.NoError(t, json.Unmarshal([]byte(`{"type":"response.refusal.done","refusal":"I cannot help with that."}`), &event))
	acc := NewBufferedResponseAccumulator()
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.refusal.delta", Delta: "I cannot"})
	acc.ProcessEvent(&event)
	response := &ResponsesResponse{Status: "completed"}
	acc.SupplementResponseOutput(response)
	choice := ResponsesToChatCompletions(response, "public").Choices[0]
	require.Equal(t, event.Refusal, choice.Message.Refusal)
	require.Equal(t, "content_filter", choice.FinishReason)

	state := NewResponsesEventToChatState()
	chunks := ResponsesEventToChatChunks(&event, state)
	require.Len(t, chunks, 1)
	require.Equal(t, &event.Refusal, chunks[0].Choices[0].Delta.Refusal)
	require.Empty(t, ResponsesEventToChatChunks(&event, state))
	chunks = ResponsesEventToChatChunks(&ResponsesStreamEvent{Type: "response.completed", Response: response}, state)
	require.Len(t, chunks, 1)
	require.NotNil(t, chunks[0].Choices[0].FinishReason)
	require.Equal(t, "content_filter", *chunks[0].Choices[0].FinishReason)
}
