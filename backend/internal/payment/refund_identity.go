package payment

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"strings"
)

// AlipayRefundRequestID is stable for this project's single refund action per
// order (successful partial refunds are terminal too). Never use for multiple
// independent partial-refund actions without a persisted action identity.
func AlipayRefundRequestID(orderID, amount string) (string, error) {
	orderID = strings.TrimSpace(orderID)
	value, ok := new(big.Rat).SetString(strings.TrimSpace(amount))
	if !ok || value.Sign() <= 0 || orderID == "" {
		return "", fmt.Errorf("invalid refund identity")
	}
	cents := new(big.Rat).Mul(value, big.NewRat(100, 1))
	if !cents.IsInt() {
		return "", fmt.Errorf("refund amount must be whole cents")
	}
	digest := sha256.Sum256([]byte(orderID + ":" + cents.Num().String()))
	id := fmt.Sprintf("%s-refund-v1-%x", orderID, digest[:8])
	if len(id) > 64 {
		return "", fmt.Errorf("refund request number exceeds provider limit")
	}
	return id, nil
}
