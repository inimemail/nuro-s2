package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesInputItemRoundTripPreservesStructuredFunctionOutput(t *testing.T) {
	for _, input := range []string{
		`{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"ok"}]}`,
		`{"type":"function_call_output","call_id":"call-1","output":{"ok":true,"count":2}}`,
	} {
		var item ResponsesInputItem
		require.NoError(t, json.Unmarshal([]byte(input), &item))

		wire, err := json.Marshal(item)
		require.NoError(t, err)
		require.True(t, json.Valid(wire))
		require.JSONEq(t, input, string(wire))
	}
}

func TestResponsesInputItemRoundTripPreservesStringFunctionOutput(t *testing.T) {
	input := `{"type":"function_call_output","call_id":"call-1","output":"plain text"}`
	var item ResponsesInputItem
	require.NoError(t, json.Unmarshal([]byte(input), &item))

	wire, err := json.Marshal(item)
	require.NoError(t, err)
	require.JSONEq(t, input, string(wire))
}
