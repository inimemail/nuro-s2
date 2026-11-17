package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type alipayRefundAttempt struct {
	RequestID     string `json:"requestID"`
	RefundAmount  string `json:"refundAmount"`
	GatewayAmount string `json:"gatewayAmount"`
	Currency      string `json:"currency"`
}

func pinAlipayRefundAttempt(ctx context.Context, tx *dbent.Tx, p *RefundPlan) error {
	amount := formatGatewayRefundAmount(p.GatewayAmount, p.Order)
	id, err := payment.AlipayRefundRequestID(p.Order.OutTradeNo, amount)
	if err != nil {
		return err
	}
	candidate := alipayRefundAttempt{id, payment.FormatAmountForCurrency(p.RefundAmount, PaymentOrderCurrency(p.Order)), amount, PaymentOrderCurrency(p.Order)}
	oid := strconv.FormatInt(p.OrderID, 10)
	entry, err := tx.PaymentAuditLog.Query().Where(paymentauditlog.OrderIDEQ(oid), paymentauditlog.ActionEQ("ALIPAY_REFUND_ATTEMPT")).Order(paymentauditlog.ByCreatedAt(sql.OrderDesc()), paymentauditlog.ByID(sql.OrderDesc())).First(ctx)
	if err == nil {
		var previous alipayRefundAttempt
		if err := json.Unmarshal([]byte(entry.Detail), &previous); err != nil {
			return fmt.Errorf("parse refund identity: %w", err)
		}
		if previous != candidate {
			return infraerrors.Conflict("REFUND_IDENTITY_MISMATCH", "retry must use the original refund amount and currency")
		}
		p.gatewayRequestID = previous.RequestID
		p.queryBeforeRefund = true
		p.gatewayRequestedAt = entry.CreatedAt
		return nil
	}
	if !dbent.IsNotFound(err) {
		return fmt.Errorf("load refund identity: %w", err)
	}
	// Old failed/unknown refunds used timestamp IDs. Without a stored request
	// number their nonexecution cannot be proved, so require manual reconciliation.
	legacy, err := tx.PaymentAuditLog.Query().Where(paymentauditlog.OrderIDEQ(oid), paymentauditlog.ActionIn("REFUND_GATEWAY_FAILED", "REFUND_FAILED", "REFUND_PENDING")).Exist(ctx)
	if err != nil {
		return err
	}
	if legacy {
		return infraerrors.Conflict("REFUND_RECONCILIATION_REQUIRED", "legacy refund requires reconciliation before retry")
	}
	body, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	entry, err = tx.PaymentAuditLog.Create().SetOrderID(oid).SetAction("ALIPAY_REFUND_ATTEMPT").SetDetail(string(body)).SetOperator("admin").Save(ctx)
	if err != nil {
		return fmt.Errorf("persist refund identity: %w", err)
	}
	if _, err := tx.PaymentOrder.UpdateOneID(p.OrderID).SetRefundAmount(p.RefundAmount).SetRefundReason(p.Reason).Save(ctx); err != nil {
		return err
	}
	p.gatewayRequestID = id
	p.gatewayRequestedAt = entry.CreatedAt
	return nil
}
