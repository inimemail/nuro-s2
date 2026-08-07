//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSchedulerRequestGuardsRejectCancelledContextBeforeSnapshotRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	accounts, mixed, err := listSchedulableAccountsForRequest(ctx, nil, nil, PlatformOpenAI, false)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, accounts)
	require.False(t, mixed)

	account, err := getSchedulerAccountForRequest(ctx, nil, 1)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, account)
}
