package repository

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

func TestRestoreRefundBalancePreservesRechargeTotalAndTransaction(t *testing.T) {
	repo, client := newUserEntRepo(t)
	ctx := context.Background()
	user, err := client.User.Create().SetEmail("refund-restore@example.com").SetPasswordHash("hash").SetBalance(10).SetTotalRecharged(100).Save(ctx)
	require.NoError(t, err)

	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.RestoreRefundBalance(dbent.NewTxContext(ctx, tx), user.ID, 25))
	held, err := tx.User.Get(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, 35.0, held.Balance)
	require.Equal(t, 100.0, held.TotalRecharged)
	require.NoError(t, tx.Rollback())

	stored, err := client.User.Get(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, 10.0, stored.Balance, "compensation must roll back with order finalization")
	require.NoError(t, repo.RestoreRefundBalance(ctx, user.ID, 25))
	stored, err = client.User.Get(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, 35.0, stored.Balance)
	require.Equal(t, 100.0, stored.TotalRecharged, "returning held funds is not a new recharge")
	require.Error(t, repo.RestoreRefundBalance(ctx, user.ID, -1))
}
