//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseOpenCodeGoUsageLimitResetDuration(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    time.Duration
	}{
		{"days", "Weekly usage limit reached. Resets in 2 days.", 48 * time.Hour},
		{"compound", "Usage exhausted; resets in 1 hour 30 minutes.", 90 * time.Minute},
		{"compound with and", "Usage exhausted; resets in 1 hour and 30 minutes.", 90 * time.Minute},
		{"weeks", "Usage exhausted. Resets in 1 week.", 7 * 24 * time.Hour},
		{"missing marker", "Weekly usage limit reached.", 0},
		{"invalid unit", "Weekly usage limit reached. Resets in 2 fortnights.", 0},
		{"overflow", "Weekly usage limit reached. Resets in 999999999999999999999 days.", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, parseOpenCodeGoUsageLimitResetDuration(tt.message))
		})
	}
}

func TestParseOpenAIRateLimitResetTimeUsesGoUsageMessage(t *testing.T) {
	before := time.Now().Add(48 * time.Hour).Unix()
	resetAt := parseOpenAIRateLimitResetTime([]byte(`{"error":{"type":"GoUsageLimitError","message":"Weekly usage limit reached. Resets in 2 days."}}`))
	after := time.Now().Add(48 * time.Hour).Unix()
	require.NotNil(t, resetAt)
	require.GreaterOrEqual(t, *resetAt, before)
	require.LessOrEqual(t, *resetAt, after)
}
