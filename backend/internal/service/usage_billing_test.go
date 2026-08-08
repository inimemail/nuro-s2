package service

import (
	"math"
	"testing"
)

func TestQuantizeUsageBillingAmountKeepsDatabasePrecision(t *testing.T) {
	value := 0.0000000099
	got := quantizeUsageBillingAmount(value)
	if got != value {
		t.Fatalf("quantizeUsageBillingAmount(%g) = %g, want %g", value, got, value)
	}

	if got := quantizeUsageBillingAmount(math.NaN()); !math.IsNaN(got) {
		t.Fatalf("NaN should remain NaN, got %v", got)
	}
}
