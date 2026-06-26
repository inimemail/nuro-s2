package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

func TestCalculateGatewayPaymentAmountAppliesRechargeMultiplier(t *testing.T) {
	require.InDelta(t, 50.00, calculateGatewayPaymentAmount(100, 2, payment.DefaultPaymentCurrency), 0.0001)
	require.InDelta(t, 100.00, calculateGatewayPaymentAmount(100, 0, payment.DefaultPaymentCurrency), 0.0001)
}

func TestCalculateCreateOrderPayAmountForOrderBalanceUsesGatewayAmount(t *testing.T) {
	_, payAmount, err := calculateCreateOrderPayAmountForOrder(payment.OrderTypeBalance, 100, 0, 2, payment.DefaultPaymentCurrency)
	require.NoError(t, err)
	require.InDelta(t, 50.00, payAmount, 0.0001)

	_, subscriptionPayAmount, err := calculateCreateOrderPayAmountForOrder(payment.OrderTypeSubscription, 100, 0, 2, payment.DefaultPaymentCurrency)
	require.NoError(t, err)
	require.InDelta(t, 100.00, subscriptionPayAmount, 0.0001)
}
