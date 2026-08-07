//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

type subscriptionRenewalLockRepoStub struct {
	UserSubscriptionRepository
	locked     *UserSubscription
	lockCalls  int
	extendedTo time.Time
}

func (r *subscriptionRenewalLockRepoStub) GetByIDForUpdate(context.Context, int64) (*UserSubscription, error) {
	r.lockCalls++
	copy := *r.locked
	return &copy, nil
}

func (r *subscriptionRenewalLockRepoStub) ExtendExpiry(_ context.Context, _ int64, expiresAt time.Time) error {
	r.extendedTo = expiresAt
	return nil
}

func TestSubscriptionRenewalUsesLockedExpirySnapshot(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	lockedExpiry := now.Add(48 * time.Hour)
	repo := &subscriptionRenewalLockRepoStub{locked: &UserSubscription{
		ID: 19, Status: SubscriptionStatusActive, ExpiresAt: lockedExpiry,
	}}
	svc := &SubscriptionService{userSubRepo: repo, now: func() time.Time { return now }}

	err := svc.updateExistingSubscriptionTerm(context.Background(), 19, 7, "")
	require.NoError(t, err)
	require.Equal(t, 1, repo.lockCalls)
	require.Equal(t, lockedExpiry.AddDate(0, 0, 7), repo.extendedTo)
}

func TestSubscriptionUpdateReusesOuterTransaction(t *testing.T) {
	client := newPaymentConfigServiceTestClient(t)
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	svc := &SubscriptionService{entClient: client}
	txCtx := dbent.NewTxContext(context.Background(), tx)
	called := false
	err = svc.withSubscriptionUpdateTx(txCtx, func(ctx context.Context) error {
		called = true
		require.NotNil(t, dbent.TxFromContext(ctx), "nested subscription updates must reuse the caller transaction")
		return nil
	})
	require.NoError(t, err)
	require.True(t, called)
}
