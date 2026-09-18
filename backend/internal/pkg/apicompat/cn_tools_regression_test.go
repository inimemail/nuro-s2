package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCNToolsToolSearchDiscovery(t *testing.T) {
	var req ResponsesRequest
	err := json.Unmarshal([]byte(`{"tools":[{"type":"tool_search"}],"input":[{"type":"tool_search_call","call_id":"s1","arguments":{"query":"workspace"}},{"type":"tool_search_output","call_id":"s1","status":"completed","execution":"client","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}]}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ResponsesToChatCompletionsRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range out.Tools {
		if tool.Function != nil && tool.Function.Name == "exec" {
			found = true
		}
	}
	if !found {
		t.Error("discovered exec is missing from upstream tools")
	}
	for _, msg := range out.Messages {
		if msg.Role == "tool" {
			var payload string
			require.NoError(t, json.Unmarshal(msg.Content, &payload))
			require.Contains(t, payload, `"name":"exec"`)
		}
	}
}

func TestCNToolsAliasDoesNotStealFunctionAndStreamsCustom(t *testing.T) {
	for _, declaredFunction := range []bool{false, true} {
		state := NewChatCompletionsToResponsesStreamState("test")
		state.CustomTools = map[string]bool{"exec": true}
		state.FunctionTools = map[string]bool{"functions__exec": declaredFunction}
		state.NamespaceTools = map[string]NamespacedToolName{"functions__wait": {Namespace: "functions", Name: "wait"}}
		idx := 0
		for _, args := range []string{`{"input":`, `"pwd"}`} {
			ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{{Index: &idx, ID: "c1", Function: ChatFunctionCall{Name: "functions__exec", Arguments: args}}}}}}}, state)
		}
		require.NoError(t, state.ValidateToolCallArguments())
		events := FinalizeChatCompletionsResponsesStream(state)
		out := events[len(events)-1].Response.Output
		require.Len(t, out, 1)
		if declaredFunction {
			require.Equal(t, "function_call", out[0].Type)
			require.Equal(t, "functions__exec", out[0].Name)
		} else {
			require.Equal(t, "custom_tool_call", out[0].Type)
			require.Equal(t, "exec", out[0].Name)
			require.Equal(t, "pwd", out[0].Input)
		}
	}
}

func TestCNToolsReasoningStopsAtNewUserTurn(t *testing.T) {
	req := &ResponsesRequest{Input: json.RawMessage(`[{"type":"reasoning","summary":[{"type":"summary_text","text":"first turn"}]},{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"ok"},{"role":"user","content":"new task"},{"type":"function_call","call_id":"b","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"b","output":"ok"}]`)}
	out, err := ResponsesToChatCompletionsRequest(req)
	require.NoError(t, err)
	seen := false
	for _, m := range out.Messages {
		for _, call := range m.ToolCalls {
			if call.ID == "b" {
				seen = true
				require.Empty(t, m.ReasoningContent)
			}
		}
	}
	require.True(t, seen)
}

func TestCNToolsCompletedArgumentsDoNotDuplicateOrOverwrite(t *testing.T) {
	for _, final := range []string{`{"cmd":"pwd"}`, `{"cmd":"ls"}`} {
		state := NewResponsesEventToChatState()
		ResponsesEventToChatChunks(&ResponsesStreamEvent{Type: "response.output_item.added", Item: &ResponsesOutput{Type: "function_call", CallID: "c", Name: "exec"}}, state)
		ResponsesEventToChatChunks(&ResponsesStreamEvent{Type: "response.function_call_arguments.delta", Delta: `{"cmd":"pwd"}`}, state)
		require.Empty(t, ResponsesEventToChatChunks(&ResponsesStreamEvent{Type: "response.function_call_arguments.done", Arguments: final}, state))
	}
}

func TestCNToolsChainedReasoning(t *testing.T) {
	req := &ResponsesRequest{Input: json.RawMessage(`[{"type":"reasoning","summary":[{"type":"summary_text","text":"turn thinking"}]},{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"ok"},{"type":"function_call","call_id":"b","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"b","output":"ok"}]`)}
	out, err := ResponsesToChatCompletionsRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range out.Messages {
		for _, call := range msg.ToolCalls {
			if msg.ReasoningContent != "turn thinking" {
				t.Errorf("call %s lost turn reasoning: %q", call.ID, msg.ReasoningContent)
			}
		}
	}
}

func TestCNToolsCustomToolAlias(t *testing.T) {
	out := ChatCompletionsResponseToResponses(&ChatCompletionsResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "c1", Type: "function", Function: ChatFunctionCall{Name: "functions__exec", Arguments: `{"input":"pwd"}`}}}}}}}, "test", map[string]bool{"exec": true}, false, map[string]NamespacedToolName{"functions__wait": {Namespace: "functions", Name: "wait"}})
	if len(out.Output) != 1 || out.Output[0].Type != "custom_tool_call" || out.Output[0].Name != "exec" {
		t.Errorf("custom tool alias not restored: %+v", out.Output)
	}
}

func TestCNToolsDiscoveryPreservesSchemaAndRejectsConflicts(t *testing.T) {
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"tools":[{"type":"tool_search"}],"input":[{"type":"tool_search_output","call_id":"s1","status":"completed","tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{"id":{"type":"integer","maximum":9007199254740993}}}}]}]}`), &req))
	tools, err := EffectiveResponsesTools(&req)
	require.NoError(t, err)
	require.Len(t, tools, 2)
	require.Contains(t, string(tools[1].Parameters), "9007199254740993")
	req.Tools = append(req.Tools, ResponsesTool{Type: "function", Name: "exec", Parameters: json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}}}`)})
	_, err = EffectiveResponsesTools(&req)
	require.ErrorContains(t, err, "conflicts")
	// A history item without a declared tool-search tool must not add tools.
	req.Tools = nil
	tools, err = EffectiveResponsesTools(&req)
	require.NoError(t, err)
	require.Empty(t, tools)
}

func TestCNToolsReasoningDoesNotBecomeAnswer(t *testing.T) {
	marker := "<｜DSML｜invoke name=\"exec\">pwd</｜DSML｜invoke>"
	out := ChatCompletionsResponseToResponses(&ChatCompletionsResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", ReasoningContent: marker}, FinishReason: "stop"}}}, "test", nil, false, nil)
	for _, item := range out.Output {
		if item.Type == "message" {
			t.Errorf("reasoning copied into answer: %+v", item.Content)
		}
	}
}
