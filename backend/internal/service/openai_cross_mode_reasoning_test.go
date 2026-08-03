package service

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeOpenAICrossModeFailoverReasoning_DropsEncryptedItemsOnlyFromRetryBody(t *testing.T) {
	original := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"keep"},{"type":"reasoning","encrypted_content":"secret"},{"type":"reasoning","summary":[{"type":"summary_text","text":"keep summary"}]}]}`)
	canonical := append([]byte(nil), original...)

	retry, changed, err := SanitizeOpenAICrossModeFailoverReasoning(original)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, canonical, original, "canonical request must not be mutated")

	var body map[string]any
	require.NoError(t, json.Unmarshal(retry, &body))
	items := body["input"].([]any)
	require.Len(t, items, 2)
	require.Equal(t, "message", items[0].(map[string]any)["type"])
	require.Equal(t, "reasoning", items[1].(map[string]any)["type"])
}

func TestSanitizeOpenAICrossModeFailoverReasoning_IsNoOpForSafeBodies(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"model":"gpt-5.5","input":[{"type":"reasoning","summary":[]}]}`),
		[]byte(`{"model":"gpt-5.5","input":[{"type":"message","content":"hello"}]}`),
		[]byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`),
	} {
		original := append([]byte(nil), body...)
		retry, changed, err := SanitizeOpenAICrossModeFailoverReasoning(body)
		require.NoError(t, err)
		require.False(t, changed)
		require.True(t, bytes.Equal(original, retry))
		require.Equal(t, original, body)
	}
}

func TestTrimOpenAIEncryptedReasoningItems_DropsEncryptedCompactionItems(t *testing.T) {
	for _, itemType := range []string{"compaction", "compaction_summary"} {
		t.Run(itemType, func(t *testing.T) {
			body := map[string]any{"input": []any{
				map[string]any{"id": "cmp_stale", "type": itemType, "encrypted_content": "gAAA"},
				map[string]any{"type": "message", "content": "keep"},
			}}

			require.True(t, trimOpenAIEncryptedReasoningItems(body))
			items := body["input"].([]any)
			require.Len(t, items, 1)
			require.Equal(t, "message", items[0].(map[string]any)["type"])
		})
	}

	body := map[string]any{"input": []any{
		map[string]any{"id": "cmp_plain", "type": "compaction", "summary": []any{}},
	}}
	require.False(t, trimOpenAIEncryptedReasoningItems(body))
}
