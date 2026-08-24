package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeResponsesStreamingTerminalOutputPreservesReportedItems(t *testing.T) {
	doneItems := newResponsesStreamOutputItems()
	doneItems.Observe([]byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","status":"completed","phase":"final_answer","vendor":"keep"}}`))
	doneItems.Observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"opaque"}}`))

	normalized, changed := normalizeResponsesStreamingTerminalOutput(
		[]byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`),
		doneItems,
	)
	require.True(t, changed)
	require.Len(t, gjson.GetBytes(normalized, "response.output").Array(), 2)
	require.Equal(t, "rs_1", gjson.GetBytes(normalized, "response.output.0.id").String())
	require.Equal(t, "opaque", gjson.GetBytes(normalized, "response.output.0.encrypted_content").String())
	require.Equal(t, "msg_1", gjson.GetBytes(normalized, "response.output.1.id").String())
	require.Equal(t, "keep", gjson.GetBytes(normalized, "response.output.1.vendor").String())
}

func TestNormalizeResponsesStreamingTerminalOutputLeavesCompleteOutputAlone(t *testing.T) {
	doneItems := newResponsesStreamOutputItems()
	doneItems.Observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message"}}`))
	raw := []byte(`{"type":"response.completed","response":{"output":[{"id":"upstream","type":"message"}]}}`)
	normalized, changed := normalizeResponsesStreamingTerminalOutput(raw, doneItems)
	require.False(t, changed)
	require.Equal(t, raw, normalized)
}

func TestNormalizeResponsesStreamingTerminalOutputIgnoresNonDoneEvents(t *testing.T) {
	doneItems := newResponsesStreamOutputItems()
	doneItems.Observe([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`))
	raw := []byte(`{"type":"response.completed","response":{"output":[]}}`)
	normalized, changed := normalizeResponsesStreamingTerminalOutput(raw, doneItems)
	require.False(t, changed)
	require.Equal(t, raw, normalized)
}
