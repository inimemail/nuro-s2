package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages_IsolatedFromAPIKeyScalars(t *testing.T) {
	stages, err := NormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages(map[string]any{
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:      800,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 5000,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
			map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			map[string]any{"stage": 2, "placeholder_ms": 3000, "guard_max_ms": 10000},
		},
		openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey:      100000,
		openAIAPIKeyFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 100000,
	})
	require.NoError(t, err)
	require.Equal(t, 800, stages[0].PlaceholderMS)
	require.Equal(t, 5000, stages[0].GuardMaxMS)
	require.Equal(t, 3000, stages[1].PlaceholderMS)
}
