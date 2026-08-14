package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages_IsolatedFromAPIKeyScalars(t *testing.T) {
	stages, err := NormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages(map[string]any{
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:         800,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 5000,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
			map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			map[string]any{"stage": 2, "placeholder_ms": 3000, "guard_max_ms": 10000},
		},
		openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey:         100000,
		openAIAPIKeyFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 100000,
	})
	require.NoError(t, err)
	require.Equal(t, 800, stages[0].PlaceholderMS)
	require.Equal(t, 5000, stages[0].GuardMaxMS)
	require.Equal(t, 3000, stages[1].PlaceholderMS)
}

func TestNormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages_PreservesDefaultFourStages(t *testing.T) {
	stages, err := NormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages(map[string]any{
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:         800,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 5000,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
			map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			map[string]any{"stage": 2, "placeholder_ms": 3000, "guard_max_ms": 10000},
			map[string]any{"stage": 3, "placeholder_ms": 5000, "guard_max_ms": 15000},
			map[string]any{"stage": 4, "placeholder_ms": 10000, "guard_max_ms": 30000},
		},
	})
	require.NoError(t, err)
	require.Equal(t, defaultOpenAIAPIKeyFirstTokenTimeoutPlaceholderStages(), stages)
}

func TestNormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages_RepairsLastGuardAsLegacyStageOne(t *testing.T) {
	stages, err := NormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages(map[string]any{
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:         800,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 30000,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
			map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			map[string]any{"stage": 2, "placeholder_ms": 3000, "guard_max_ms": 10000},
			map[string]any{"stage": 3, "placeholder_ms": 5000, "guard_max_ms": 15000},
			map[string]any{"stage": 4, "placeholder_ms": 10000, "guard_max_ms": 30000},
		},
	})
	require.NoError(t, err)
	require.Equal(t, defaultOpenAIAPIKeyFirstTokenTimeoutPlaceholderStages(), stages)
}

func TestNormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages_PreservesValidLegacyScalarAuthority(t *testing.T) {
	stages, err := NormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages(map[string]any{
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:         900,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 6000,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey: []map[string]any{
			{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			{"stage": 2, "placeholder_ms": 3000, "guard_max_ms": 10000},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 900, stages[0].PlaceholderMS)
	require.Equal(t, 6000, stages[0].GuardMaxMS)
}
