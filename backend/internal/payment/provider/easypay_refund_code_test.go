package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

func TestEasyPayRefundSuccessCodeIsStrictAndPerInstance(t *testing.T) {
	for _, code := range []string{"", "0", "1"} {
		p, err := NewEasyPay("fixture", map[string]string{"pid": "test", "pkey": "fixture", "notifyUrl": "https://example.com/notify", "returnUrl": "https://example.com/return", "apiBase": "https://example.com", "refundSuccessCode": code})
		require.NoError(t, err)
		expected := 1
		if code == "0" {
			expected = 0
		}
		for _, tc := range []struct {
			body    string
			success bool
		}{
			{`{"code":0}`, expected == 0}, {`{"code":1}`, expected == 1}, {`{"code":"0"}`, expected == 0}, {`{"code":"1"}`, expected == 1},
			{`{"code":1.9}`, false}, {`{"code":null}`, false}, {`{}`, false}, {`{"code":true}`, false}, {`{"code":"1.0"}`, false},
		} {
			require.Equal(t, tc.success, parseEasyPayRefundResponse(200, []byte(tc.body), expected) == nil, code+tc.body)
		}
		require.NotNil(t, p)
	}
	for _, code := range []string{"2", "-1", "1.9", "null", "true"} {
		_, err := NewEasyPay("fixture", map[string]string{"pid": "test", "pkey": "fixture", "notifyUrl": "https://example.com/notify", "returnUrl": "https://example.com/return", "apiBase": "https://example.com", "refundSuccessCode": code})
		require.Error(t, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`)) }))
	defer server.Close()
	a, err := NewEasyPay("a", map[string]string{"pid": "test", "pkey": "fixture", "notifyUrl": "https://example.com/notify", "returnUrl": "https://example.com/return", "apiBase": server.URL, "refundSuccessCode": "0"})
	require.NoError(t, err)
	b, err := NewEasyPay("b", map[string]string{"pid": "test", "pkey": "fixture", "notifyUrl": "https://example.com/notify", "returnUrl": "https://example.com/return", "apiBase": server.URL})
	require.NoError(t, err)
	_, err = a.Refund(context.Background(), payment.RefundRequest{OrderID: "order", Amount: "1.00"})
	require.NoError(t, err)
	_, err = b.Refund(context.Background(), payment.RefundRequest{OrderID: "order", Amount: "1.00"})
	require.Error(t, err)
	require.False(t, easyPayResponseCodeIsSuccess(float64(0)), "creation and notification keep success code 1")
}
