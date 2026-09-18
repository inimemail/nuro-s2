package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const cnToolMetadataRequest = `{"input":"inspect workspace","tools":[{"type":"custom","name":"exec","description":"Run JavaScript with the provided tools API.","format":{"type":"grammar","syntax":"lark","definition":"start: /[a-z]+/"}},{"type":"namespace","name":"functions","description":"Use the workspace directory; tool arguments are JSON.","tools":[{"type":"function","name":"wait","description":"Wait for a running command.","parameters":{"type":"object","properties":{"cell_id":{"type":"string"}}}}]}]}`

func TestCNToolMetadataPreservedAcrossBridges(t *testing.T) {
	for _, protocol := range []string{"chat", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			var req ResponsesRequest
			var descriptions []string
			if protocol == "chat" {
				require.NoError(t, json.Unmarshal([]byte(cnToolMetadataRequest), &req))
				out, err := ResponsesToChatCompletionsRequest(&req)
				require.NoError(t, err)
				require.Len(t, out.Tools, 2)
				require.Equal(t, "exec", out.Tools[0].Function.Name)
				require.Equal(t, "functions__wait", out.Tools[1].Function.Name)
				for _, tool := range out.Tools {
					descriptions = append(descriptions, tool.Function.Description)
				}
			} else {
				var body map[string]any
				require.NoError(t, json.Unmarshal([]byte(cnToolMetadataRequest), &body))
				_, changed, err := AdaptResponsesClientTools(body)
				require.NoError(t, err)
				require.True(t, changed)
				encoded, err := json.Marshal(body)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(encoded, &req))
				out, err := ResponsesToAnthropicRequest(&req)
				require.NoError(t, err)
				require.Len(t, out.Tools, 2)
				for _, tool := range out.Tools {
					descriptions = append(descriptions, tool.Description)
				}
			}
			require.Contains(t, descriptions[0], "Run JavaScript")
			require.Contains(t, descriptions[0], "start: /[a-z]+/")
			require.Contains(t, descriptions[0], "lark")
			require.Contains(t, descriptions[0], `"input"`)
			require.Contains(t, descriptions[1], "Use the workspace directory")
			require.Contains(t, descriptions[1], "Wait for a running command.")
		})
	}
}

func TestCNToolMetadataInheritedDeclarationsStayStable(t *testing.T) {
	var req map[string]any
	require.NoError(t, json.Unmarshal([]byte(cnToolMetadataRequest), &req))
	mapping, _, err := AdaptResponsesClientTools(req)
	require.NoError(t, err)
	initial, err := json.Marshal(req["tools"])
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		next := map[string]any{"input": "continue"}
		mapping, _, err = AdaptResponsesClientToolsWithInheritedMapping(next, mapping, req["tools"].([]any))
		require.NoError(t, err)
		got, err := json.Marshal(next["tools"])
		require.NoError(t, err)
		require.JSONEq(t, string(initial), string(got))
		req = next
	}
}

func TestCNToolMetadataDiscoveryDoesNotLoseGrammarConflicts(t *testing.T) {
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"tools":[{"type":"tool_search"},{"type":"custom","name":"exec","format":{"type":"grammar","syntax":"lark","definition":"start: /[a-z]+/"}}],"input":[{"type":"tool_search_output","call_id":"s1","tools":[{"type":"custom","name":"exec","format":{"type":"grammar","syntax":"lark","definition":"start: /[0-9]+/"}}]}]}`), &req))
	_, err := EffectiveResponsesTools(&req)
	require.ErrorContains(t, err, "conflicts")
}

func TestCNToolMetadataChatRejectsCustomFunctionNameCollision(t *testing.T) {
	for _, tools := range [][]ResponsesTool{
		{{Type: "custom", Name: "exec"}, {Type: "function", Name: "exec"}},
		{{Type: "function", Name: "exec"}, {Type: "custom", Name: "exec"}},
	} {
		_, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{Input: json.RawMessage(`"hi"`), Tools: tools})
		require.ErrorContains(t, err, "conflicts")
	}
}

func TestCNToolMetadataInheritedSearchAcceptsSameDeclarations(t *testing.T) {
	var original map[string]any
	require.NoError(t, json.Unmarshal([]byte(cnToolMetadataRequest), &original))
	declarations := original["tools"].([]any)
	original["tools"] = append([]any{map[string]any{"type": "tool_search"}}, declarations...)
	mapping, _, err := AdaptResponsesClientTools(original)
	require.NoError(t, err)
	before, err := json.Marshal(original["tools"])
	require.NoError(t, err)
	next := map[string]any{"input": []any{map[string]any{
		"type": "tool_search_output", "call_id": "search1", "status": "completed", "tools": declarations,
	}}}
	_, _, err = AdaptResponsesClientToolsWithInheritedMapping(next, mapping, original["tools"].([]any))
	require.NoError(t, err)
	after, err := json.Marshal(next["tools"])
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
}

func TestCNToolMetadataKeepsOrdinaryToolsAndExplicitNone(t *testing.T) {
	const body = `{"input":"hi","tool_choice":"none","tools":[{"type":"function","name":"exec","description":"Keep exactly this description.","parameters":{"type":"object","properties":{"id":{"type":"integer","maximum":9007199254740993}}}}]}`
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	chat, err := ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, err)
	require.Equal(t, `"none"`, string(chat.ToolChoice))
	require.Equal(t, req.Tools[0].Description, chat.Tools[0].Function.Description)
	require.JSONEq(t, string(req.Tools[0].Parameters), string(chat.Tools[0].Function.Parameters))
	var raw map[string]any
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&raw))
	_, changed, err := AdaptResponsesClientTools(raw)
	require.NoError(t, err)
	require.False(t, changed)
	encoded, err := json.Marshal(raw)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "9007199254740993")
	require.JSONEq(t, body, string(encoded))
}

func TestCNToolMetadataTextIsNeverPromotedToExecutableCall(t *testing.T) {
	text := `<tool_call>functions.exec command="pwd"</tool_call>`
	content, err := json.Marshal(text)
	require.NoError(t, err)
	out := ChatCompletionsResponseToResponses(&ChatCompletionsResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: content}, FinishReason: "stop"}}}, "test", map[string]bool{"exec": true}, false, nil)
	require.Len(t, out.Output, 1)
	require.Equal(t, "message", out.Output[0].Type)
	require.Equal(t, text, out.Output[0].Content[0].Text)
}
