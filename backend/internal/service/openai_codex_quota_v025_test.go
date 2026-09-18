package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAICodexWindowResetAtAnchorsRelativeReset(t *testing.T) {
	updated := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	extra := map[string]any{
		"codex_usage_updated_at":       updated.Format(time.RFC3339),
		"codex_5h_reset_after_seconds": 300,
	}
	resetAt, ok := openAICodexWindowResetAt(extra, "5h")
	require.True(t, ok)
	require.Equal(t, updated.Add(5*time.Minute), resetAt)
	require.False(t, openAIQuotaWindowReset(extra, "5h", updated.Add(4*time.Minute)))
	require.True(t, openAIQuotaWindowReset(extra, "5h", updated.Add(6*time.Minute)))
}

func TestOpenAICanonicalQuotaWindowsPreferNormalizedFields(t *testing.T) {
	now := time.Now().UTC()
	extra := map[string]any{
		"codex_usage_updated_at": now.Format(time.RFC3339),
		"codex_5h_used_percent":  12.0,
		"codex_7d_used_percent":  34.0,
	}
	fiveHour, sevenDay := openAICanonicalQuotaWindows(extra, now)
	require.True(t, fiveHour.hasUsed)
	require.True(t, sevenDay.hasUsed)
	require.Equal(t, 12.0, fiveHour.usedPercent)
	require.Equal(t, 34.0, sevenDay.usedPercent)
}
