//go:build unit

package antigravity

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUpgradeNonnegativeOrdinaryInputSyncAndStream(t *testing.T) {
	for _, prompt := range []int{0, 3, 10, 20} {
		raw := map[string]any{"response": map[string]any{"candidates": []any{map[string]any{"finishReason": "STOP", "content": map[string]any{"parts": []any{map[string]any{"text": "ok"}}}}}, "usageMetadata": map[string]any{"promptTokenCount": prompt, "cachedContentTokenCount": 10, "candidatesTokenCount": 2}}}
		body, err := json.Marshal(raw)
		require.NoError(t, err)
		_, usage, err := TransformGeminiToClaude(body, "gemini-test")
		require.NoError(t, err)
		expected := prompt - 10
		if expected < 0 {
			expected = 0
		}
		require.Equal(t, expected, usage.InputTokens)
		require.Equal(t, 10, usage.CacheReadInputTokens)
		stream := NewStreamingProcessor("gemini-test")
		stream.ProcessLine("data: " + string(body))
		_, streamUsage := stream.Finish()
		require.Equal(t, expected, streamUsage.InputTokens)
		require.Equal(t, 10, streamUsage.CacheReadInputTokens)
	}
}
