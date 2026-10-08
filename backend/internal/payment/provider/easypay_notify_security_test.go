package provider

// Regression tests for the EasyPay forged-callback vulnerability
// (Wei-Shaw/sub2api issue #7875).
//
// Attack recap: the order-creation signature (visible to the payer in the
// submit.php popup URL) signs return_url among other fields, and the sign
// base string concatenates values unescaped. A client-supplied return_url
// ending in "&trade_status=TRADE_SUCCESS" therefore produced a signature
// that was byte-identical to a payment-success notification's signature
// once the callback decoded return_url partially and promoted the smuggled
// pair to a top-level parameter. Merchant-key rotation could not help
// because the attacker never needed the key.
//
// Fixes covered here:
//  1. service.CanonicalizeReturnURL drops client-supplied query parameters
//     (see payment_resume_service_test.go).
//  2. VerifyNotification rejects any parameter outside the genuine async
//     notify set, so smuggled order-creation fields (return_url et al.)
//     fail closed even if a signed value smuggles a fake pair.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

// easyPayPoCProvider returns a provider with a fixed test credential set.
func easyPayPoCProvider() *EasyPay {
	return &EasyPay{config: map[string]string{
		"pid":       "1000",
		"pkey":      "MERCHANT_SECRET_KEY",
		"apiBase":   "https://pay.example.com",
		"notifyUrl": "https://site.example.com/api/v1/payment/webhook/easypay",
		"returnUrl": "https://site.example.com/payment/result",
	}}
}

func TestEasyPayNotifyRejectsAmbiguousParams(t *testing.T) {
	e := easyPayPoCProvider()
	params := map[string]string{"pid": "1000", "trade_no": "T1", "out_trade_no": "ORDER123", "type": "alipay", "name": "recharge", "money": "650.00", "trade_status": tradeStatusSuccess}
	q := url.Values{}
	for key, value := range params {
		q.Set(key, value)
	}
	q.Set("sign", easyPaySign(params, e.config["pkey"]))
	q.Set("sign_type", signTypeMD5)
	for _, field := range []string{"sign", "money", "pid", "trade_status"} {
		for _, prepend := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prepend=%v", field, prepend), func(t *testing.T) {
				duplicate := field + "=" + url.QueryEscape(q.Get(field))
				raw := q.Encode() + "&" + duplicate
				if prepend {
					raw = duplicate + "&" + q.Encode()
				}
				if _, err := e.VerifyNotification(context.Background(), raw, nil); err == nil {
					t.Fatal("duplicate accepted")
				}
			})
		}
	}
	if _, err := e.VerifyNotification(context.Background(), q.Encode()+"&param=%zz", nil); err == nil {
		t.Fatal("malformed encoding accepted")
	}
}

func TestEasyPayQueryRejectsInvalidPaidResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"rejected", `{"code":0,"status":1,"money":"650.00","msg":"secret"}`, false},
		{"invalid", `{"code":1,"status":1,"money":"broken"}`, false},
		{"zero", `{"code":1,"status":1,"money":"0"}`, false},
		{"negative", `{"code":1,"status":1,"money":"-1"}`, false},
		{"nan", `{"code":1,"status":1,"money":"NaN"}`, false},
		{"inf", `{"code":1,"status":1,"money":"Inf"}`, false},
		{"paid", `{"code":1,"status":1,"money":"650.00"}`, true},
		{"pending", `{"code":1,"status":0,"money":""}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer server.Close()
			_, err := newTestEasyPay(t, server.URL).QueryOrder(context.Background(), "ORDER123")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
		})
	}
}

func TestEasyPayQueryRejectsHTTPFailureAndOversizedResponse(t *testing.T) {
	paid := `{"code":1,"status":1,"money":"650.00"}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"HTTP failure", http.StatusInternalServerError, paid},
		{"oversized valid JSON prefix", http.StatusOK, paid + strings.Repeat(" ", maxEasypayResponseSize-len(paid)) + "INVALID_TAIL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			_, err := newTestEasyPay(t, server.URL).QueryOrder(context.Background(), "ORDER123")
			if err == nil {
				t.Fatal("invalid transport response must not report a paid order")
			}
		})
	}
}

// TestEasyPayNotifyRejectsForgedSignReuseCallback is the exact PoC payload
// that was accepted before the fix: the order's own submit.php signature
// replayed with trade_status smuggled out of the return_url value.
func TestEasyPayNotifyRejectsForgedSignReuseCallback(t *testing.T) {
	t.Parallel()
	e := easyPayPoCProvider()

	// Order creation (popup mode): return_url carries the smuggled pair as
	// its last sorted inner query key, exactly as buildPaymentReturnURL
	// would have produced before CanonicalizeReturnURL stripped user query.
	returnURL := "https://site.example.com/payment/result?order_id=99&out_trade_no=ORDER123&status=success&trade_status=TRADE_SUCCESS"
	createParams := map[string]string{
		"pid":          "1000",
		"type":         "alipay",
		"out_trade_no": "ORDER123",
		"notify_url":   e.config["notifyUrl"],
		"return_url":   returnURL,
		"name":         "balance recharge",
		"money":        "650.00",
	}
	sign := easyPaySign(createParams, e.config["pkey"])

	// Forged callback: return_url encoded only up to status=success, then a
	// raw &trade_status=TRADE_SUCCESS promotes it to a top-level parameter.
	prefix := "https://site.example.com/payment/result?order_id=99&out_trade_no=ORDER123&status=success"
	cb := url.Values{}
	cb.Set("pid", "1000")
	cb.Set("type", "alipay")
	cb.Set("out_trade_no", "ORDER123")
	cb.Set("notify_url", e.config["notifyUrl"])
	cb.Set("name", "balance recharge")
	cb.Set("money", "650.00")
	cb.Set("return_url", prefix)
	rawCallback := cb.Encode() + "&trade_status=TRADE_SUCCESS" + "&sign=" + sign + "&sign_type=MD5"

	if _, err := e.VerifyNotification(context.Background(), rawCallback, nil); err == nil {
		t.Fatal("forged sign-reuse callback must be rejected")
	}
}

// TestEasyPayNotifyRejectsOrderURLReplay covers the milder variant where the
// attacker replays the complete signed pay URL unchanged: return_url itself
// is not a legitimate notify parameter and must also be rejected.
func TestEasyPayNotifyRejectsOrderURLReplay(t *testing.T) {
	t.Parallel()
	e := easyPayPoCProvider()

	createParams := map[string]string{
		"pid":          "1000",
		"type":         "alipay",
		"out_trade_no": "ORDER123",
		"notify_url":   e.config["notifyUrl"],
		"return_url":   "https://site.example.com/payment/result",
		"name":         "balance recharge",
		"money":        "650.00",
	}
	createParams["sign"] = easyPaySign(createParams, e.config["pkey"])
	createParams["sign_type"] = signTypeMD5
	q := url.Values{}
	for k, v := range createParams {
		q.Set(k, v)
	}

	if _, err := e.VerifyNotification(context.Background(), q.Encode(), nil); err == nil {
		t.Fatal("replayed order URL must be rejected")
	}
}

// TestEasyPayNotifyAcceptsGenuineCallback locks in the legitimate notify
// contract: the canonical parameter set with a valid signature succeeds.
func TestEasyPayNotifyAcceptsGenuineCallback(t *testing.T) {
	t.Parallel()
	e := easyPayPoCProvider()

	params := map[string]string{
		"pid":          "1000",
		"trade_no":     "2026100622001400000001",
		"out_trade_no": "ORDER123",
		"type":         "alipay",
		"name":         "balance recharge",
		"money":        "650.00",
		"trade_status": tradeStatusSuccess,
	}
	sign := easyPaySign(params, e.config["pkey"])
	params["sign"] = sign
	params["sign_type"] = signTypeMD5
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}

	n, err := e.VerifyNotification(context.Background(), q.Encode(), nil)
	if err != nil {
		t.Fatalf("genuine callback rejected: %v", err)
	}
	if n.Status != payment.ProviderStatusSuccess {
		t.Fatalf("status = %v, want success", n.Status)
	}
	if n.OrderID != "ORDER123" || n.TradeNo != "2026100622001400000001" || n.Amount != 650.00 {
		t.Fatalf("unexpected notification: %+v", n)
	}
}

// TestEasyPayNotifyRejectsUnknownParam ensures any parameter outside the
// canonical notify set fails closed, including empty-valued ones that the
// signer itself would skip.
func TestEasyPayNotifyRejectsUnknownParam(t *testing.T) {
	t.Parallel()
	e := easyPayPoCProvider()

	params := map[string]string{
		"pid":          "1000",
		"trade_no":     "T1",
		"out_trade_no": "ORDER123",
		"type":         "alipay",
		"name":         "balance recharge",
		"money":        "650.00",
		"trade_status": tradeStatusSuccess,
	}
	sign := easyPaySign(params, e.config["pkey"])
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	q.Set("sign", sign)
	q.Set("sign_type", signTypeMD5)
	q.Set("device", "") // empty value: invisible to the signer, still rejected

	if _, err := e.VerifyNotification(context.Background(), q.Encode(), nil); err == nil {
		t.Fatal("callback with unknown param must be rejected")
	}
}
