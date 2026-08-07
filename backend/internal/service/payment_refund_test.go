//go:build unit

package service

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type refundAvailableBalanceRepoStub struct {
	UserRepository
	user          *User
	deducted      float64
	deductRequest float64
	deductCalls   int
	deductInTx    bool
}

func (r *refundAvailableBalanceRepoStub) GetByID(context.Context, int64) (*User, error) {
	return r.user, nil
}

func (r *refundAvailableBalanceRepoStub) DeductAvailableBalance(ctx context.Context, _ int64, amount float64) (float64, error) {
	r.deductCalls++
	r.deductInTx = dbent.TxFromContext(ctx) != nil
	r.deductRequest = amount
	r.deducted = math.Min(amount, math.Max(r.user.Balance, 0))
	return r.deducted, nil
}

func (r *refundAvailableBalanceRepoStub) DeductAvailableBalanceExact(ctx context.Context, _ int64, amount float64) (float64, error) {
	if r.user.Balance < amount {
		return 0, nil
	}
	r.deductCalls++
	r.deductInTx = dbent.TxFromContext(ctx) != nil
	r.deductRequest = amount
	r.deducted = amount
	return amount, nil
}

type refundPartialOnlyBalanceRepoStub struct {
	UserRepository
	deductCalls int
}

func (r *refundPartialOnlyBalanceRepoStub) DeductAvailableBalance(context.Context, int64, float64) (float64, error) {
	r.deductCalls++
	return 2, nil
}

func TestFinalizePendingRefundClaimsOrderBeforeDeduction(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	user, err := client.User.Create().
		SetEmail("pending-refund@example.com").
		SetPasswordHash("hash").
		SetUsername("pending-refund-user").
		Save(ctx)
	require.NoError(t, err)
	order, err := client.PaymentOrder.Create().
		SetUserID(user.ID).
		SetUserEmail(user.Email).
		SetUserName(user.Username).
		SetAmount(20).
		SetPayAmount(20).
		SetFeeRate(0).
		SetRechargeCode("PENDING-REFUND").
		SetOutTradeNo("pending-refund-order").
		SetPaymentTradeNo("pending-refund-trade").
		SetPaymentType(payment.TypeStripe).
		SetOrderType(payment.OrderTypeBalance).
		SetStatus(OrderStatusRefundPending).
		SetRefundAmount(20).
		SetExpiresAt(time.Now().Add(time.Hour)).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com").
		Save(ctx)
	require.NoError(t, err)

	repo := &refundAvailableBalanceRepoStub{user: &User{ID: user.ID, Balance: 20}}
	svc := &PaymentService{entClient: client, userRepo: repo}
	plan := &RefundPlan{
		OrderID: order.ID, Order: order, RefundAmount: 20,
		BalanceToDeduct: 20, DeductionType: payment.DeductionTypeBalance,
	}

	result, err := svc.finalizePendingRefundSuccess(ctx, plan)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, 1, repo.deductCalls)
	require.True(t, repo.deductInTx, "pending refund deduction must share the order finalization transaction")
	stored, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusRefunded, stored.Status)
	require.Equal(t, 1, client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(order.ID, 10)), paymentauditlog.ActionEQ("REFUND_SUCCESS")).
		CountX(ctx))

	_, err = svc.finalizePendingRefundSuccess(ctx, plan)
	require.Error(t, err)
	require.Equal(t, 1, repo.deductCalls, "a stale second finalization must fail before deduction")
}

func TestRefundBalanceDeductionRequiresExplicitForce(t *testing.T) {
	repo := &refundAvailableBalanceRepoStub{user: &User{ID: 7, Balance: 3}}
	svc := &PaymentService{userRepo: repo}
	order := &dbent.PaymentOrder{UserID: 7, OrderType: payment.OrderTypeBalance}
	plan := &RefundPlan{RefundAmount: 5}

	result := svc.prepDeduct(context.Background(), order, plan, false)
	require.NotNil(t, result)
	require.True(t, result.RequireForce)
	require.Zero(t, plan.BalanceToDeduct)

	result = svc.prepDeduct(context.Background(), order, plan, true)
	require.Nil(t, result)
	require.Equal(t, 3.0, plan.BalanceToDeduct)
}

func TestRefundUsesAtomicAvailableBalanceDeductionResult(t *testing.T) {
	repo := &refundAvailableBalanceRepoStub{user: &User{ID: 7, Balance: 2}}
	svc := &PaymentService{userRepo: repo}

	deducted, err := svc.deductAvailableBalance(context.Background(), 7, 5)
	require.NoError(t, err)
	require.Equal(t, 5.0, repo.deductRequest)
	require.Equal(t, 2.0, deducted)
}

func TestFinalizePendingRefundRejectsInsufficientNonForceBalance(t *testing.T) {
	repo := &refundAvailableBalanceRepoStub{user: &User{ID: 7, Balance: 2}}
	svc := &PaymentService{userRepo: repo}
	plan := &RefundPlan{
		Order:           &dbent.PaymentOrder{UserID: 7},
		BalanceToDeduct: 5,
		DeductionType:   payment.DeductionTypeBalance,
		Force:           false,
	}

	err := svc.applyRefundFinalDeduction(context.Background(), plan)
	require.Error(t, err)
	require.Zero(t, repo.deductCalls)
	require.Equal(t, 5.0, plan.BalanceToDeduct)
}

func TestRefundBalanceDeductionDoesNotContinueAfterConcurrentShortfall(t *testing.T) {
	// Non-force refunds fail closed for adapters that cannot provide the exact
	// atomic operation. A partial deduction must never reach the gateway.
	repo := &refundPartialOnlyBalanceRepoStub{}
	svc := &PaymentService{userRepo: repo}

	deducted, err := svc.deductRefundBalance(context.Background(), 7, 5, true)
	require.Error(t, err)
	require.Zero(t, deducted)
	require.Zero(t, repo.deductCalls)
}

func TestValidateRefundRequestRejectsLegacyGuessedProviderInstance(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)

	user, err := client.User.Create().
		SetEmail("refund-legacy@example.com").
		SetPasswordHash("hash").
		SetUsername("refund-legacy-user").
		Save(ctx)
	require.NoError(t, err)

	_, err = client.PaymentProviderInstance.Create().
		SetProviderKey(payment.TypeAlipay).
		SetName("alipay-refund-instance").
		SetConfig("{}").
		SetSupportedTypes("alipay").
		SetEnabled(true).
		SetAllowUserRefund(true).
		SetRefundEnabled(true).
		Save(ctx)
	require.NoError(t, err)

	order, err := client.PaymentOrder.Create().
		SetUserID(user.ID).
		SetUserEmail(user.Email).
		SetUserName(user.Username).
		SetAmount(88).
		SetPayAmount(88).
		SetFeeRate(0).
		SetRechargeCode("REFUND-LEGACY-ORDER").
		SetOutTradeNo("sub2_refund_legacy_order").
		SetPaymentType(payment.TypeAlipay).
		SetPaymentTradeNo("trade-legacy-refund").
		SetOrderType(payment.OrderTypeBalance).
		SetStatus(OrderStatusCompleted).
		SetExpiresAt(time.Now().Add(time.Hour)).
		SetPaidAt(time.Now()).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com").
		Save(ctx)
	require.NoError(t, err)

	svc := &PaymentService{
		entClient: client,
	}

	_, err = svc.validateRefundRequest(ctx, order.ID, user.ID)
	require.Error(t, err)
	require.Equal(t, "USER_REFUND_DISABLED", infraerrors.Reason(err))
}

func TestPrepareRefundRejectsLegacyGuessedProviderInstance(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)

	user, err := client.User.Create().
		SetEmail("refund-legacy-admin@example.com").
		SetPasswordHash("hash").
		SetUsername("refund-legacy-admin-user").
		Save(ctx)
	require.NoError(t, err)

	_, err = client.PaymentProviderInstance.Create().
		SetProviderKey(payment.TypeAlipay).
		SetName("alipay-refund-admin-instance").
		SetConfig("{}").
		SetSupportedTypes("alipay").
		SetEnabled(true).
		SetAllowUserRefund(true).
		SetRefundEnabled(true).
		Save(ctx)
	require.NoError(t, err)

	order, err := client.PaymentOrder.Create().
		SetUserID(user.ID).
		SetUserEmail(user.Email).
		SetUserName(user.Username).
		SetAmount(188).
		SetPayAmount(188).
		SetFeeRate(0).
		SetRechargeCode("REFUND-LEGACY-ADMIN-ORDER").
		SetOutTradeNo("sub2_refund_legacy_admin_order").
		SetPaymentType(payment.TypeAlipay).
		SetPaymentTradeNo("trade-legacy-admin-refund").
		SetOrderType(payment.OrderTypeBalance).
		SetStatus(OrderStatusCompleted).
		SetExpiresAt(time.Now().Add(time.Hour)).
		SetPaidAt(time.Now()).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com").
		Save(ctx)
	require.NoError(t, err)

	svc := &PaymentService{
		entClient: client,
	}

	plan, result, err := svc.PrepareRefund(ctx, order.ID, 0, "", false, false)
	require.Nil(t, plan)
	require.Nil(t, result)
	require.Error(t, err)
	require.Equal(t, "REFUND_DISABLED", infraerrors.Reason(err))
}

func TestPrepDeductBalanceRequiresForceWhenBalanceIsInsufficient(t *testing.T) {
	tests := []struct {
		name         string
		balance      float64
		force        bool
		wantDeduct   float64
		requireForce bool
	}{
		{name: "insufficient balance", balance: 40, requireForce: true},
		{name: "forced insufficient balance", balance: 40, force: true, wantDeduct: 40},
		{name: "equal balance", balance: 100, wantDeduct: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := &RefundPlan{RefundAmount: 100}
			svc := &PaymentService{userRepo: &mockUserRepo{getByIDUser: &User{Balance: tt.balance}}}
			result := svc.prepDeduct(context.Background(), &dbent.PaymentOrder{
				UserID: 1, OrderType: payment.OrderTypeBalance,
			}, plan, tt.force)
			if tt.requireForce {
				require.NotNil(t, result)
				require.True(t, result.RequireForce)
				require.Zero(t, plan.BalanceToDeduct)
				return
			}
			require.Nil(t, result)
			require.Equal(t, tt.wantDeduct, plan.BalanceToDeduct)
		})
	}
}

func TestDeductAvailableBalanceUsesAtomicRepositoryResult(t *testing.T) {
	var requested float64
	svc := &PaymentService{userRepo: &mockUserRepo{
		deductAvailableBalanceFn: func(_ context.Context, id int64, amount float64) (float64, error) {
			require.Equal(t, int64(7), id)
			requested = amount
			return 25, nil
		},
	}}
	deducted, err := svc.deductAvailableBalance(context.Background(), 7, 100)
	require.NoError(t, err)
	require.Equal(t, 100.0, requested)
	require.Equal(t, 25.0, deducted)
}

func TestGwRefundRejectsAlipayMerchantIdentitySnapshotMismatch(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)

	user, err := client.User.Create().
		SetEmail("refund-snapshot-mismatch@example.com").
		SetPasswordHash("hash").
		SetUsername("refund-snapshot-mismatch-user").
		Save(ctx)
	require.NoError(t, err)

	inst, err := client.PaymentProviderInstance.Create().
		SetProviderKey(payment.TypeAlipay).
		SetName("alipay-refund-mismatch-instance").
		SetConfig(encryptWebhookProviderConfig(t, map[string]string{
			"appId":      "runtime-alipay-app",
			"privateKey": "runtime-private-key",
		})).
		SetSupportedTypes("alipay").
		SetEnabled(true).
		SetRefundEnabled(true).
		Save(ctx)
	require.NoError(t, err)

	instID := strconv.FormatInt(inst.ID, 10)
	order, err := client.PaymentOrder.Create().
		SetUserID(user.ID).
		SetUserEmail(user.Email).
		SetUserName(user.Username).
		SetAmount(88).
		SetPayAmount(88).
		SetFeeRate(0).
		SetRechargeCode("REFUND-SNAPSHOT-MISMATCH-ORDER").
		SetOutTradeNo("sub2_refund_snapshot_mismatch_order").
		SetPaymentType(payment.TypeAlipay).
		SetPaymentTradeNo("trade-refund-snapshot-mismatch").
		SetOrderType(payment.OrderTypeBalance).
		SetStatus(OrderStatusCompleted).
		SetExpiresAt(time.Now().Add(time.Hour)).
		SetPaidAt(time.Now()).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com").
		SetProviderInstanceID(instID).
		SetProviderKey(payment.TypeAlipay).
		SetProviderSnapshot(map[string]any{
			"schema_version":       2,
			"provider_instance_id": instID,
			"provider_key":         payment.TypeAlipay,
			"merchant_app_id":      "expected-alipay-app",
		}).
		Save(ctx)
	require.NoError(t, err)

	svc := &PaymentService{
		entClient:    client,
		loadBalancer: newWebhookProviderTestLoadBalancer(client),
	}

	_, err = svc.gwRefund(ctx, &RefundPlan{
		OrderID:       order.ID,
		Order:         order,
		RefundAmount:  order.Amount,
		GatewayAmount: order.Amount,
		Reason:        "snapshot mismatch",
	})
	require.ErrorContains(t, err, "alipay app_id mismatch")
}

func TestCalculateGatewayRefundAmountUsesCurrencyPrecision(t *testing.T) {
	require.InDelta(t, 6.173, calculateGatewayRefundAmount(100, 12.345, 50, "KWD"), 1e-12)
	require.InDelta(t, 12.345, calculateGatewayRefundAmount(100, 12.345, 100, "KWD"), 1e-12)
	require.InDelta(t, 52, calculateGatewayRefundAmount(100, 103, 50, "JPY"), 1e-12)
}

func TestFormatGatewayRefundAmountUsesOrderCurrency(t *testing.T) {
	order := &dbent.PaymentOrder{
		ProviderSnapshot: map[string]any{
			"currency": "KWD",
		},
	}

	require.Equal(t, "12.345", formatGatewayRefundAmount(12.345, order))
}

func TestValidateRefundProviderResponseAcceptsPending(t *testing.T) {
	require.NoError(t, validateRefundProviderResponse(&payment.RefundResponse{Status: payment.ProviderStatusPending}))
	require.NoError(t, validateRefundProviderResponse(&payment.RefundResponse{Status: payment.ProviderStatusSuccess}))
	require.Error(t, validateRefundProviderResponse(&payment.RefundResponse{Status: payment.ProviderStatusFailed}))
	require.Error(t, validateRefundProviderResponse(nil))
}
