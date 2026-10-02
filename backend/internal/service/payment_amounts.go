package service

import (
	"math"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/shopspring/decimal"
)

const defaultBalanceRechargeMultiplier = 1.0

func normalizeBalanceRechargeMultiplier(multiplier float64) float64 {
	if math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || multiplier <= 0 {
		return defaultBalanceRechargeMultiplier
	}
	return multiplier
}

func normalizeSubscriptionUSDToCNYRate(rate float64) float64 {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
		return 0
	}
	return rate
}

func calculateCreditedBalance(paymentAmount, multiplier float64) float64 {
	return decimal.NewFromFloat(paymentAmount).
		Mul(decimal.NewFromFloat(normalizeBalanceRechargeMultiplier(multiplier))).
		Round(2).
		InexactFloat64()
}

// calculateBalanceBonusAmount returns only the promotional portion of a
// balance recharge. Subscription orders and discounts never create a bonus;
// keeping this derived from the rounded credited amount makes the stored order
// value reconcile exactly with the amount later redeemed.
func calculateBalanceBonusAmount(paymentAmount, creditedAmount, multiplier float64) float64 {
	if multiplier <= 1 || paymentAmount <= 0 || creditedAmount <= paymentAmount {
		return 0
	}
	return decimal.NewFromFloat(creditedAmount).
		Sub(decimal.NewFromFloat(paymentAmount)).
		Round(2).
		InexactFloat64()
}

func calculateGatewayPaymentAmount(orderAmount, multiplier float64, currency string) float64 {
	fractionDigits := int32(payment.CurrencyMaxFractionDigits(currency))
	return decimal.NewFromFloat(orderAmount).
		Div(decimal.NewFromFloat(normalizeBalanceRechargeMultiplier(multiplier))).
		Round(fractionDigits).
		InexactFloat64()
}

func calculateGatewayRefundAmount(orderAmount, payAmount, refundAmount float64, currency string) float64 {
	if orderAmount <= 0 || payAmount <= 0 || refundAmount <= 0 {
		return 0
	}
	fractionDigits := int32(payment.CurrencyMaxFractionDigits(currency))
	if math.Abs(refundAmount-orderAmount) <= paymentAmountToleranceForCurrency(currency) {
		return decimal.NewFromFloat(payAmount).Round(fractionDigits).InexactFloat64()
	}
	return decimal.NewFromFloat(payAmount).
		Mul(decimal.NewFromFloat(refundAmount)).
		Div(decimal.NewFromFloat(orderAmount)).
		Round(fractionDigits).
		InexactFloat64()
}
