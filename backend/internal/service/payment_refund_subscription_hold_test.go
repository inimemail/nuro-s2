//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type refundSubscriptionRepo struct {
	UserSubscriptionRepository
	sub          UserSubscription
	duplicate    bool
	restoreCalls int
	restoreInTx  bool
}

func (r *refundSubscriptionRepo) GetByID(context.Context, int64) (*UserSubscription, error) {
	if r.sub.DeletedAt != nil {
		return nil, ErrSubscriptionNotFound
	}
	copy := r.sub
	return &copy, nil
}

func (r *refundSubscriptionRepo) Delete(context.Context, int64) error {
	now := time.Now()
	r.sub.DeletedAt = &now
	return nil
}

func (r *refundSubscriptionRepo) GetByIDIncludeDeleted(context.Context, int64) (*UserSubscription, error) {
	copy := r.sub
	return &copy, nil
}

func (r *refundSubscriptionRepo) ExistsByUserIDAndGroupID(context.Context, int64, int64) (bool, error) {
	return r.duplicate || r.sub.DeletedAt == nil, nil
}

func (r *refundSubscriptionRepo) Restore(ctx context.Context, _ int64, status string) (*UserSubscription, error) {
	r.restoreCalls++
	r.restoreInTx = dbent.TxFromContext(ctx) != nil
	r.sub.DeletedAt = nil
	r.sub.Status = status
	copy := r.sub
	return &copy, nil
}

func TestRefundPendingRevokedSubscriptionReturnsOriginalTerm(t *testing.T) {
	for _, status := range []string{payment.ProviderStatusFailed, payment.ProviderStatusSuccess} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			client := newPaymentConfigServiceTestClient(t)
			order := createCompletedRefundableOrderForTest(t, ctx, client, "sub-hold")
			groupID := int64(81)
			order, err := client.PaymentOrder.UpdateOneID(order.ID).
				SetOrderType(payment.OrderTypeSubscription).SetSubscriptionGroupID(groupID).SetSubscriptionDays(30).Save(ctx)
			require.NoError(t, err)
			expiry := time.Now().Add(48 * time.Hour)
			repo := &refundSubscriptionRepo{sub: UserSubscription{ID: 19, UserID: order.UserID, GroupID: groupID,
				Status: SubscriptionStatusActive, ExpiresAt: expiry, DailyUsageUSD: 12.5}}
			provider := &pendingRefundProviderTestDouble{queryStatus: status}
			restore := replacePaymentProviderFactoryForTest(t, provider)
			defer restore()
			svc := &PaymentService{entClient: client, loadBalancer: &captureLoadBalancer{},
				subscriptionSvc: &SubscriptionService{userSubRepo: repo}}
			plan := &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 100, GatewayAmount: 100,
				DeductBalance: true, DeductionType: payment.DeductionTypeSubscription, SubDaysToDeduct: 30, SubscriptionID: repo.sub.ID}
			pending, err := svc.ExecuteRefund(ctx, plan)
			require.NoError(t, err)
			require.False(t, pending.Success)
			require.NotNil(t, repo.sub.DeletedAt)
			detail, err := svc.latestRefundPendingDetail(ctx, order.ID)
			require.NoError(t, err)
			require.True(t, detail.SubscriptionRevoked, "revocation must survive a process restart via the pending audit")

			result, err := svc.QueryAndFinalizeRefund(ctx, order.ID)
			require.NoError(t, err)
			require.NotContains(t, result.Warning, "rollback failed")
			require.Equal(t, status == payment.ProviderStatusSuccess, result.Success)
			require.Equal(t, expiry, repo.sub.ExpiresAt, "return must preserve expiry, rather than adding the purchased days again")
			require.Equal(t, 12.5, repo.sub.DailyUsageUSD, "return must preserve usage counters")
			if status == payment.ProviderStatusFailed {
				require.Nil(t, repo.sub.DeletedAt)
				require.Equal(t, 1, repo.restoreCalls)
				require.True(t, repo.restoreInTx, "return must share the order finalization transaction")
			} else {
				require.NotNil(t, repo.sub.DeletedAt)
				require.Zero(t, repo.restoreCalls)
			}
			_, err = svc.QueryAndFinalizeRefund(ctx, order.ID)
			require.Equal(t, "INVALID_STATUS", infraerrors.Reason(err))
		})
	}
}

func TestRefundRevokedSubscriptionRollbackPreservesDuplicateGuard(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		name := "restores"
		if duplicate {
			name = "duplicate conflict"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			client := newPaymentConfigServiceTestClient(t)
			order := createCompletedRefundableOrderForTest(t, ctx, client, "sub-rollback")
			now := time.Now()
			expiry := now.Add(48 * time.Hour)
			repo := &refundSubscriptionRepo{duplicate: duplicate, sub: UserSubscription{ID: 19, UserID: order.UserID,
				GroupID: 81, Status: SubscriptionStatusActive, ExpiresAt: expiry, DeletedAt: &now}}
			svc := &PaymentService{entClient: client, subscriptionSvc: &SubscriptionService{userSubRepo: repo}}
			plan := &RefundPlan{OrderID: order.ID, Order: order, DeductionType: payment.DeductionTypeSubscription,
				SubDaysToDeduct: 30, SubscriptionID: repo.sub.ID, subscriptionRevoked: true}
			require.Equal(t, !duplicate, svc.RollbackRefund(ctx, plan, errors.New("gateway failed")))
			require.Equal(t, expiry, repo.sub.ExpiresAt)
			if duplicate {
				require.NotNil(t, repo.sub.DeletedAt, "never revive a second subscription for the same group")
				require.Zero(t, repo.restoreCalls)
				require.Equal(t, 1, countRefundAuditsForTest(t, ctx, client, order.ID, "REFUND_ROLLBACK_FAILED"))
			} else {
				require.Nil(t, repo.sub.DeletedAt)
				require.Equal(t, 1, repo.restoreCalls)
			}
		})
	}
}
