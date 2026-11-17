//go:build unit

package service

import (
	"context"
	"errors"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
	"strconv"
	"testing"
	"time"
)

func TestV0222RefundIdentityPersistsBeforeRetryAndRejectsAmountChange(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	order := createPendingRefundOrderForTest(t, ctx, client, "v0222-identity")
	_, err := client.PaymentAuditLog.Delete().Exec(ctx)
	require.NoError(t, err)
	plan := &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 20, GatewayAmount: 20, Reason: "test"}
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, pinAlipayRefundAttempt(ctx, tx, plan))
	require.NotEmpty(t, plan.gatewayRequestID)
	require.False(t, plan.queryBeforeRefund)
	require.NoError(t, tx.Commit())
	key := plan.gatewayRequestID
	retry := &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 20, GatewayAmount: 20}
	tx, err = client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, pinAlipayRefundAttempt(ctx, tx, retry))
	require.Equal(t, key, retry.gatewayRequestID)
	require.True(t, retry.queryBeforeRefund)
	require.NoError(t, tx.Rollback())
	retry.RefundAmount = 21
	retry.GatewayAmount = 21
	tx, err = client.Tx(ctx)
	require.NoError(t, err)
	require.ErrorContains(t, pinAlipayRefundAttempt(ctx, tx, retry), "original refund amount")
	require.NoError(t, tx.Rollback())
	saved, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, 20.0, saved.RefundAmount)
}
func TestV0222RefundIdentityRollsBackWithTransaction(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	order := createPendingRefundOrderForTest(t, ctx, client, "v0222-rollback")
	_, err := client.PaymentAuditLog.Delete().Exec(ctx)
	require.NoError(t, err)
	plan := &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 10, GatewayAmount: 10}
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, pinAlipayRefundAttempt(ctx, tx, plan))
	require.NoError(t, tx.Rollback())
	require.Zero(t, client.PaymentAuditLog.Query().CountX(ctx))
}

type v0222AlipayProvider struct {
	refundProviderTestDouble
	queryStatus string
	queryError  error
	queries     []payment.RefundQueryRequest
	refunds     []payment.RefundRequest
}

func (*v0222AlipayProvider) ProviderKey() string { return payment.TypeAlipay }
func (p *v0222AlipayProvider) QueryRefund(_ context.Context, req payment.RefundQueryRequest) (*payment.RefundResponse, error) {
	p.queries = append(p.queries, req)
	return &payment.RefundResponse{RefundID: req.RefundID, Status: p.queryStatus}, p.queryError
}
func (p *v0222AlipayProvider) Refund(_ context.Context, req payment.RefundRequest) (*payment.RefundResponse, error) {
	p.refunds = append(p.refunds, req)
	return &payment.RefundResponse{RefundID: req.RequestID, Status: payment.ProviderStatusPending}, nil
}
func TestV0222AlipayRetryQueriesBeforeSendingAndUnknownStaysPending(t *testing.T) {
	for _, status := range []string{payment.ProviderStatusSuccess, payment.ProviderStatusPending, payment.ProviderStatusFailed, "unknown", "query_error"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			client := newPaymentConfigServiceTestClient(t)
			order := createCompletedRefundableOrderForTest(t, ctx, client, "v0222-"+status)
			iid, err := strconv.ParseInt(*order.ProviderInstanceID, 10, 64)
			require.NoError(t, err)
			_, err = client.PaymentProviderInstance.UpdateOneID(iid).SetProviderKey(payment.TypeAlipay).SetSupportedTypes(payment.TypeAlipay).Save(ctx)
			require.NoError(t, err)
			order, err = client.PaymentOrder.UpdateOneID(order.ID).SetPaymentType(payment.TypeAlipay).Save(ctx)
			require.NoError(t, err)
			provider := &v0222AlipayProvider{queryStatus: status}
			if status == "query_error" {
				provider.queryError = errors.New("unavailable")
			}
			restore := replacePaymentProviderFactoryForTest(t, provider)
			defer restore()
			svc := &PaymentService{entClient: client, loadBalancer: &captureLoadBalancer{}}
			plan := &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 20, GatewayAmount: 20}
			tx, err := client.Tx(ctx)
			require.NoError(t, err)
			require.NoError(t, pinAlipayRefundAttempt(ctx, tx, plan))
			require.NoError(t, tx.Commit())
			retry := &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 20, GatewayAmount: 20}
			tx, err = client.Tx(ctx)
			require.NoError(t, err)
			require.NoError(t, pinAlipayRefundAttempt(ctx, tx, retry))
			require.NoError(t, tx.Commit())
			response, err := svc.gwRefund(ctx, retry)
			require.NoError(t, err)
			require.Len(t, provider.queries, 1)
			require.Equal(t, plan.gatewayRequestID, provider.queries[0].RefundID)
			if status == payment.ProviderStatusFailed {
				require.Len(t, provider.refunds, 1)
				require.Equal(t, plan.gatewayRequestID, provider.refunds[0].RequestID)
			} else {
				require.Empty(t, provider.refunds)
			}
			if status == "unknown" || status == "query_error" {
				require.Equal(t, payment.ProviderStatusPending, response.Status)
			}
		})
	}
}
func TestV0222AlipayAuditFailurePreventsProviderCallAndRollsBackClaim(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	order := createCompletedRefundableOrderForTest(t, ctx, client, "v0222-audit-failure")
	iid, err := strconv.ParseInt(*order.ProviderInstanceID, 10, 64)
	require.NoError(t, err)
	_, err = client.PaymentProviderInstance.UpdateOneID(iid).SetProviderKey(payment.TypeAlipay).SetSupportedTypes(payment.TypeAlipay).Save(ctx)
	require.NoError(t, err)
	order, err = client.PaymentOrder.UpdateOneID(order.ID).SetPaymentType(payment.TypeAlipay).Save(ctx)
	require.NoError(t, err)
	provider := &v0222AlipayProvider{}
	restore := replacePaymentProviderFactoryForTest(t, provider)
	defer restore()
	client.PaymentAuditLog.Use(func(next dbent.Mutator) dbent.Mutator {
		return dbent.MutateFunc(func(ctx context.Context, m dbent.Mutation) (dbent.Value, error) {
			return nil, errors.New("audit unavailable")
		})
	})
	svc := &PaymentService{entClient: client, loadBalancer: &captureLoadBalancer{}}
	_, err = svc.ExecuteRefund(ctx, &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 20, GatewayAmount: 20})
	require.ErrorContains(t, err, "audit unavailable")
	require.Empty(t, provider.refunds)
	saved, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, saved.Status)
}

type v0222BlockedRefundProvider struct {
	v0222AlipayProvider
	started chan struct{}
	release chan struct{}
}

func (p *v0222BlockedRefundProvider) Refund(ctx context.Context, req payment.RefundRequest) (*payment.RefundResponse, error) {
	close(p.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.release:
		return &payment.RefundResponse{RefundID: req.RequestID, Status: payment.ProviderStatusPending}, nil
	}
}

func TestV0222ConcurrentRefundClaimCannotReachProviderTwice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := newPaymentConfigServiceTestClient(t)
	order := createCompletedRefundableOrderForTest(t, ctx, client, "v0222-concurrent")
	iid, err := strconv.ParseInt(*order.ProviderInstanceID, 10, 64)
	require.NoError(t, err)
	_, err = client.PaymentProviderInstance.UpdateOneID(iid).SetProviderKey(payment.TypeAlipay).SetSupportedTypes(payment.TypeAlipay).Save(ctx)
	require.NoError(t, err)
	order, err = client.PaymentOrder.UpdateOneID(order.ID).SetPaymentType(payment.TypeAlipay).Save(ctx)
	require.NoError(t, err)
	provider := &v0222BlockedRefundProvider{started: make(chan struct{}), release: make(chan struct{})}
	restore := replacePaymentProviderFactoryForTest(t, provider)
	defer restore()
	svc := &PaymentService{entClient: client, loadBalancer: &captureLoadBalancer{}}
	plan := func() *RefundPlan {
		return &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 20, GatewayAmount: 20}
	}
	done := make(chan error, 1)
	go func() { _, err := svc.ExecuteRefund(ctx, plan()); done <- err }()
	select {
	case <-provider.started:
	case <-ctx.Done():
		t.Fatal("first refund did not reach fake provider")
	}
	_, err = svc.ExecuteRefund(ctx, plan())
	require.ErrorContains(t, err, "order status changed")
	close(provider.release)
	require.NoError(t, <-done)
	require.Equal(t, 1, client.PaymentAuditLog.Query().Where(paymentauditlog.ActionEQ("ALIPAY_REFUND_ATTEMPT")).CountX(ctx))
	saved, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusRefundPending, saved.Status)
}
