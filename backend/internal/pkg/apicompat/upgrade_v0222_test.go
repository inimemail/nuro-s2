package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV0222GrokSearchFilteringIsExplicitAndPreservesClientTools(t *testing.T) {
	response := &ResponsesResponse{ID: "resp_fixture", Status: "completed", Output: []ResponsesOutput{
		{Type: "custom_tool_call", CallID: "xs_search", Name: "search", Input: "server"},
		{Type: "function_call", CallID: "xs_client", Name: "client", Arguments: `{}`},
	}}
	for _, grok := range []bool{false, true} {
		// The existing converters expose function_call; adding Grok's server
		// search filter must preserve a client function with the same prefix.
		calls := ResponsesToChatCompletions(response, "public", grok).Choices[0].Message.ToolCalls
		require.Len(t, calls, 1)
		require.Equal(t, "xs_client", calls[0].ID)
		require.Len(t, ResponsesToAnthropic(response, "public", grok).Content, 1)
	}
	search := &ResponsesStreamEvent{Type: "response.output_item.added", Item: &response.Output[0]}
	var ignored map[int]bool
	require.Same(t, search, filterGrokSearchEvent(search, false, &ignored))
	require.Empty(t, ignored)
	require.Len(t, withoutGrokServerSearch(response).Output, 1)
	require.Len(t, response.Output, 2)
	chat := NewResponsesEventToChatState()
	chat.GrokServerSideSearch = true
	anthropic := NewResponsesEventToAnthropicState()
	anthropic.GrokServerSideSearch = true
	for _, event := range []ResponsesStreamEvent{
		{Type: "response.output_item.added", OutputIndex: 0, Item: &response.Output[0]},
		{Type: "response.custom_tool_call_input.delta", OutputIndex: 0, Delta: "server"},
		{Type: "response.output_item.done", OutputIndex: 0, Item: &response.Output[0]},
	} {
		require.Empty(t, ResponsesEventToChatChunks(&event, chat))
		require.Empty(t, ResponsesEventToAnthropicEvents(&event, anthropic))
	}
	client := &ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 1, Item: &response.Output[1]}
	require.NotEmpty(t, ResponsesEventToChatChunks(client, chat))
	require.NotEmpty(t, ResponsesEventToAnthropicEvents(client, anthropic))
}

func TestV0222BufferedToolsKeepOpaqueEncryptedContent(t *testing.T) {
	acc := NewBufferedResponseAccumulator()
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.done", Item: &ResponsesOutput{
		Type: "function_call", CallID: "call_opaque", Name: "read", Arguments: `{}`, EncryptedContent: "sealed",
	}})
	output := acc.BuildOutput()
	require.Len(t, output, 1)
	require.Equal(t, "sealed", output[0].EncryptedContent)
	require.Equal(t, "call_opaque", output[0].CallID)
}

func TestV0222BufferedGrokSearchSurvivesSparseDone(t *testing.T) {
	for _, grok := range []bool{false, true} {
		for _, searchID := range []string{"xs_search", "as_search"} {
			acc := NewBufferedResponseAccumulator()
			acc.GrokServerSideSearch = grok
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 0, Item: &ResponsesOutput{
				Type: "custom_tool_call", CallID: searchID, Name: "search",
			}})
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 0, Item: &ResponsesOutput{
				Type: "custom_tool_call", Input: "server query",
			}})
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 1, Item: &ResponsesOutput{
				Type: "function_call", CallID: "xs_client", Name: "client",
			}})
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 1, Item: &ResponsesOutput{
				Type: "function_call", Arguments: `{}`,
			}})
			resp := &ResponsesResponse{ID: "resp_fixture", Status: "completed"}
			acc.SupplementResponseOutput(resp)
			require.Len(t, resp.Output, 2)
			calls := ResponsesToChatCompletions(resp, "public", grok).Choices[0].Message.ToolCalls
			if grok {
				require.Len(t, calls, 1)
				require.Equal(t, "xs_client", calls[0].ID)
				message := ResponsesToAnthropic(resp, "public", grok)
				require.Len(t, message.Content, 1)
				require.Equal(t, "xs_client", message.Content[0].ID)
			} else {
				// Preserve the accumulator's legacy custom-to-function behavior
				// when the caller has not explicitly enabled Grok filtering.
				require.Len(t, calls, 2)
				require.Equal(t, searchID, calls[0].ID)
			}
		}
	}
}

func TestV0222MidTurnSystemBecomesDeveloperWithoutReordering(t *testing.T) {
	request := &AnthropicRequest{Model: "gpt-6.1-sol", Messages: []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"first"`)},
		{Role: "system", Content: json.RawMessage(`"policy"`)},
		{Role: "user", Content: json.RawMessage(`"last"`)},
	}}
	converted, err := AnthropicToResponses(request)
	require.NoError(t, err)
	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(converted.Input, &items))
	require.Len(t, items, 3)
	require.Equal(t, "user", items[0].Role)
	require.Equal(t, "developer", items[1].Role)
	require.Equal(t, "user", items[2].Role)
}

func TestV0222StructuredOutputDefaultStrictIsFalse(t *testing.T) {
	for _, strict := range []string{"", `,"strict":true`, `,"strict":false`} {
		raw := json.RawMessage(`{"type":"json_schema","json_schema":{"name":"fixture","schema":{"type":"object"}` + strict + `}}`)
		out := chatResponseFormatToResponsesTextFormat(raw)
		require.True(t, gjson.GetBytes(out, "strict").Exists())
		require.Equal(t, strict == `,"strict":true`, gjson.GetBytes(out, "strict").Bool())
	}
}
