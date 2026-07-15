package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesToChatCompletionsRequestAdditionalToolsItem(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"custom","name":"exec","description":"Run PowerShell","format":{"type":"text"}},
				{"type":"function","name":"wait","parameters":{"type":"object","properties":{}}},
				{"type":"namespace","name":"collaboration","tools":[
					{"type":"function","name":"send_message","parameters":{"type":"object","properties":{}}}
				]}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run Get-Location"}]}
		]`),
		ToolChoice: json.RawMessage(`"auto"`),
	}

	effective, err := EffectiveResponsesTools(req)
	require.NoError(t, err)
	require.Len(t, effective, 3)
	require.True(t, CustomToolNames(effective)["exec"])
	require.Equal(t, NamespacedToolName{Namespace: "collaboration", Name: "send_message"}, NamespaceToolNames(effective)["collaboration__send_message"])

	out, err := ResponsesToChatCompletionsRequest(req)
	require.NoError(t, err)
	require.Len(t, out.Tools, 3)
	require.Equal(t, "exec", out.Tools[0].Function.Name)
	require.Equal(t, "wait", out.Tools[1].Function.Name)
	require.Equal(t, "collaboration__send_message", out.Tools[2].Function.Name)
	require.Len(t, out.Messages, 1, "additional_tools must not become a chat message")
	require.Equal(t, "user", out.Messages[0].Role)
}

func TestEffectiveResponsesToolsSkipsStringInputItems(t *testing.T) {
	req := &ResponsesRequest{
		Input: json.RawMessage(`["plain input",{"type":"additional_tools","tools":[{"type":"custom","name":"exec"}]}]`),
	}

	tools, err := EffectiveResponsesTools(req)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, "exec", tools[0].Name)
}

func TestEffectiveResponsesToolsIgnoresNonAdditionalItemToolsField(t *testing.T) {
	req := &ResponsesRequest{
		Input: json.RawMessage(`[
			{"type":"message","role":"user","tools":"not-a-tool-list"},
			{"type":"custom_item","tools":{"unexpected":"shape"}}
		]`),
	}

	tools, err := EffectiveResponsesTools(req)
	require.NoError(t, err)
	require.Empty(t, tools)
}

func TestEffectiveResponsesToolsDeduplicatesTopLevelAndAdditionalTools(t *testing.T) {
	req := &ResponsesRequest{
		Tools: []ResponsesTool{{Type: "function", Name: "exec", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Input: json.RawMessage(`[
			{"type":"additional_tools","tools":[
				{"type":"function","name":"exec","description":"run","parameters":{"type":"object"}},
				{"type":"function","name":"wait","parameters":{"type":"object"}}
			]}
		]`),
	}

	tools, err := EffectiveResponsesTools(req)
	require.NoError(t, err)
	require.Len(t, tools, 2)
	require.Equal(t, "exec", tools[0].Name)
	require.Equal(t, "wait", tools[1].Name)
}
