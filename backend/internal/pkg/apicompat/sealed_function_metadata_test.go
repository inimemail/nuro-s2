package apicompat

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSealedFunctionMetadataRoundTripAndAccumulation(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"opaque":"sealed"}`} {
		t.Run(raw, func(t *testing.T) {
			var item ResponsesOutput
			require.NoError(t, json.Unmarshal([]byte(`{"type":"function_call","call_id":"call_1","name":"tool","namespace":"agent","encrypted_function_args":`+raw+`,"arguments":"{}"}`), &item))
			body, err := json.Marshal(item)
			require.NoError(t, err)
			var got map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(body, &got))
			require.JSONEq(t, raw, string(got["encrypted_function_args"]))
			acc := NewBufferedResponseAccumulator()
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 1, Item: &ResponsesOutput{Type: "function_call", CallID: "call_1", Name: "tool"}})
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: 1, Delta: "{"})
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 1, Item: &item})
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 1, Item: &item})
			out := acc.BuildOutput()
			require.Len(t, out, 1)
			require.Equal(t, "agent", out[0].Namespace)
			require.Equal(t, "{}", out[0].Arguments)
			require.JSONEq(t, raw, string(out[0].EncryptedFunctionArgs))
			terminal := &ResponsesResponse{}
			acc.SupplementResponseOutput(terminal)
			require.Len(t, terminal.Output, 1)
			acc.SupplementResponseOutput(terminal)
			require.Len(t, terminal.Output, 1)
		})
	}
}

func TestForeignReasoningSignaturesAreNotReplayed(t *testing.T) {
	for _, sig := range []string{"gAAAA-invalid", "ccswitch-openai-reasoning-v1:sealed", "", "provider-native-signature", "ccswitch-openai-reasoning-v10:similar"} {
		raw, _ := json.Marshal([]AnthropicContentBlock{{Type: "thinking", Thinking: "reasoning", Signature: sig}, {Type: "text", Text: "answer"}})
		out, err := anthropicAssistantToResponses(raw, false)
		require.NoError(t, err)
		if sig == "provider-native-signature" || sig == "ccswitch-openai-reasoning-v10:similar" {
			require.Len(t, out, 2)
			require.Equal(t, sig, out[0].EncryptedContent)
		} else {
			require.Len(t, out, 1)
			require.Equal(t, "message", out[0].Type)
		}
	}
}

func TestBufferedToolsKeepInterleavedArgumentsAfterGrowth(t *testing.T) {
	acc := NewBufferedResponseAccumulator()
	require.NotPanics(t, func() {
		for i := 0; i < 4; i++ {
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: i, Item: &ResponsesOutput{Type: "function_call", CallID: "call_" + string(rune('a'+i)), Name: "tool"}})
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: i, Delta: `{"value":`})
		}
		for i := 0; i < 4; i++ {
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: i, Delta: `1}`})
		}
	})
	out := acc.BuildOutput()
	require.Len(t, out, 4)
	for i := range out {
		require.Equal(t, `{"value":1}`, out[i].Arguments)
	}
}

func TestBufferedCustomToolDoneKeepsCompleteInput(t *testing.T) {
	for _, withDelta := range []bool{false, true} {
		acc := NewBufferedResponseAccumulator()
		acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 2, Item: &ResponsesOutput{Type: "custom_tool_call", CallID: "call_patch", Name: "apply_patch", Namespace: "functions"}})
		if withDelta {
			acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.custom_tool_call_input.delta", OutputIndex: 2, Delta: "*** Begin"})
		}
		acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 2, Item: &ResponsesOutput{Type: "custom_tool_call", CallID: "call_patch", Name: "apply_patch", Input: "*** Begin Patch\n*** End Patch"}})
		out := acc.BuildOutput()
		require.Len(t, out, 1)
		// Retain the existing compatibility representation used by Chat/Messages.
		require.Equal(t, "function_call", out[0].Type)
		require.Equal(t, "call_patch", out[0].CallID)
		require.Equal(t, "functions", out[0].Namespace)
		require.Equal(t, "*** Begin Patch\n*** End Patch", out[0].Arguments)
	}
}
