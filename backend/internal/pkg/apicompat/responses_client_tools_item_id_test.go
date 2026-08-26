package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetypedResponsesToolCallItemID(t *testing.T) {
	tests := []struct {
		id       string
		itemType string
		want     string
	}{
		{"fc_abc", "custom_tool_call", "ctc_abc"},
		{"fc_abc", "tool_search_call", "tsc_abc"},
		{"ctc_abc", "function_call", "fc_abc"},
		{"item_abc", "custom_tool_call", "item_abc"},
		{"fc_abc", "message", "fc_abc"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, retypedResponsesToolCallItemID(tt.id, tt.itemType))
	}
}

func TestResponsesClientToolItemIDRoundTrip(t *testing.T) {
	req := map[string]any{
		"tools": []any{map[string]any{"type": "custom", "name": "exec"}},
		"input": []any{map[string]any{
			"type": "custom_tool_call", "id": "ctc_upstream1", "call_id": "call_1", "name": "exec", "input": "dir",
		}},
	}
	mapping, changed, err := AdaptResponsesClientTools(req)
	require.NoError(t, err)
	require.True(t, changed)
	input := req["input"].([]any)
	lowered := input[0].(map[string]any)
	require.Equal(t, "function_call", lowered["type"])
	require.Equal(t, "fc_upstream1", lowered["id"])

	payload, err := json.Marshal(map[string]any{"output": []any{lowered}})
	require.NoError(t, err)
	restored, changed, err := RestoreResponsesClientToolPayload(payload, mapping)
	require.NoError(t, err)
	require.True(t, changed)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(restored, &decoded))
	output := decoded["output"].([]any)[0].(map[string]any)
	require.Equal(t, "custom_tool_call", output["type"])
	require.Equal(t, "ctc_upstream1", output["id"])
}

func TestResponsesClientToolStreamRestorerKeepsUpstreamMatchID(t *testing.T) {
	restorer := NewResponsesClientToolStreamRestorer(ResponsesClientToolMapping{CustomTools: map[string]bool{"exec": true}})
	added := restorer.Restore(ResponsesStreamEvent{
		Type: "response.output_item.added", OutputIndex: 0,
		Item: &ResponsesOutput{Type: "function_call", ID: "fc_stream1", CallID: "call_1", Name: "exec"},
	})
	require.Len(t, added, 1)
	require.Equal(t, "ctc_stream1", added[0].Item.ID)

	done := restorer.Restore(ResponsesStreamEvent{
		Type: "response.function_call_arguments.done", OutputIndex: 0, ItemID: "fc_stream1",
		CallID: "call_1", Name: "exec", Arguments: `{"input":"dir"}`,
	})
	require.Len(t, done, 2)
	require.Equal(t, "ctc_stream1", done[0].ItemID)
	require.Equal(t, "ctc_stream1", done[1].ItemID)
}
