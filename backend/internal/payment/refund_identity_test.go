package payment

import "testing"

func TestRefundIdentityIsStableForCanonicalAmount(t *testing.T) {
	first, err := AlipayRefundRequestID("order_1", "12.50")
	if err != nil {
		t.Fatal(err)
	}
	for _, amount := range []string{"12.5", "012.500", " 12.50 "} {
		id, err := AlipayRefundRequestID("order_1", amount)
		if err != nil || id != first {
			t.Fatalf("%q: %q %v", amount, id, err)
		}
	}
	other, _ := AlipayRefundRequestID("order_2", "12.50")
	differentAmount, _ := AlipayRefundRequestID("order_1", "12.51")
	if first == other || first == differentAmount {
		t.Fatal("identity collision")
	}
	for _, amount := range []string{"", "0", "-1", "NaN", "Inf", "12.501"} {
		if _, err := AlipayRefundRequestID("order_1", amount); err == nil {
			t.Fatalf("accepted %q", amount)
		}
	}
}
