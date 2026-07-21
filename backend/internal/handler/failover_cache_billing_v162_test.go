package handler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFailoverCacheBillingV162WaitsForActualAccountSwitch(t *testing.T) {
	mock := &mockTempUnscheduler{}
	state := NewFailoverState(3, true)
	retryable := newTestFailoverErr(500, true, false)

	action := state.handleFailoverErrorWithRetryPlan(
		context.Background(), mock, 100, "openai", 1, 0, 0, false, retryable,
	)
	require.Equal(t, FailoverContinue, action)
	require.False(t, state.ForceCacheBilling)
	require.Zero(t, state.SwitchCount)

	action = state.handleFailoverErrorWithRetryPlan(
		context.Background(), mock, 100, "openai", 1, 0, 0, false, retryable,
	)
	require.Equal(t, FailoverContinue, action)
	require.True(t, state.ForceCacheBilling)
	require.Equal(t, 1, state.SwitchCount)
}
