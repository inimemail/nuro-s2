//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteSuccessResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name            string
		providerKey     string
		wantCode        int
		wantContentType string
		wantBody        string
		checkJSON       bool
		wantJSONCode    string
		wantJSONMessage string
	}{
		{
			name:            "wxpay returns JSON with code SUCCESS",
			providerKey:     "wxpay",
			wantCode:        http.StatusOK,
			wantContentType: "application/json",
			checkJSON:       true,
			wantJSONCode:    "SUCCESS",
			wantJSONMessage: "成功",
		},
		{
			name:            "stripe returns empty 200",
			providerKey:     "stripe",
			wantCode:        http.StatusOK,
			wantContentType: "text/plain",
			wantBody:        "",
		},
		{
			name:            "airwallex returns empty 200",
			providerKey:     payment.TypeAirwallex,
			wantCode:        http.StatusOK,
			wantContentType: "text/plain",
			wantBody:        "",
		},
		{
			name:            "easypay returns plain text success",
			providerKey:     "easypay",
			wantCode:        http.StatusOK,
			wantContentType: "text/plain",
			wantBody:        "success",
		},
		{
			name:            "alipay returns plain text success",
			providerKey:     "alipay",
			wantCode:        http.StatusOK,
			wantContentType: "text/plain",
			wantBody:        "success",
		},
		{
			name:            "unknown provider returns plain text success",
			providerKey:     "unknown_provider",
			wantCode:        http.StatusOK,
			wantContentType: "text/plain",
			wantBody:        "success",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			writeSuccessResponse(c, tt.providerKey)

			assert.Equal(t, tt.wantCode, w.Code)
			assert.Contains(t, w.Header().Get("Content-Type"), tt.wantContentType)

			if tt.checkJSON {
				var resp wxpaySuccessResponse
				err := json.Unmarshal(w.Body.Bytes(), &resp)
				require.NoError(t, err, "response body should be valid JSON")
				assert.Equal(t, tt.wantJSONCode, resp.Code)
				assert.Equal(t, tt.wantJSONMessage, resp.Message)
			} else {
				assert.Equal(t, tt.wantBody, w.Body.String())
			}
		})
	}
}

// TestUnknownOrderWebhookAcksWithSuccess exercises the response contract that
// handleNotify relies on when HandlePaymentNotification returns ErrOrderNotFound:
// we still need to emit the provider-specific 2xx so the provider stops
// retrying. We can't easily drive handleNotify end-to-end without mocking the
// concrete *service.PaymentService, so this test locks down the two ingredients
// the fix depends on:
//  1. errors.Is recognises the sentinel through fmt.Errorf %w wrapping (which
//     is how service layer wraps it with the out_trade_no context).
//  2. writeSuccessResponse produces the provider-specific body for Stripe
//     (empty 200) — matching what handleNotify calls on the ack path.
//
// If either contract breaks, the Stripe "unknown order → 500 loop" regresses.
func TestUnknownOrderWebhookAcksWithSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 1) Sentinel recognition through wrapping.
	wrapped := fmt.Errorf("%w: out_trade_no=sub2_missing_42", service.ErrOrderNotFound)
	require.True(t, errors.Is(wrapped, service.ErrOrderNotFound),
		"handleNotify uses errors.Is on the wrapped service error; regression here "+
			"would mean unknown-order webhooks go back to returning 500 and looping forever")

	// A distinct error must NOT match — otherwise a DB failure would be silently
	// swallowed as an ack.
	other := errors.New("lookup order failed: connection refused")
	require.False(t, errors.Is(other, service.ErrOrderNotFound))

	// 2) Provider-specific success body is what handleNotify emits on the
	// ack path. Asserted again here because this is the shape Stripe expects
	// to consider the webhook acknowledged.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	writeSuccessResponse(c, payment.TypeStripe)
	require.Equal(t, http.StatusOK, w.Code,
		"Stripe requires 2xx to stop retrying; anything else restarts the retry loop")
	require.Empty(t, w.Body.String(), "Stripe expects an empty body on the ack path")
}

func TestWebhookConstants(t *testing.T) {
	t.Run("maxWebhookBodySize is 1MB", func(t *testing.T) {
		assert.Equal(t, int64(1<<20), int64(maxWebhookBodySize))
	})

}

func TestWebhookVerificationFailureDoesNotLeakPayload(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		secret := "sign=SECRET_SIGNATURE&resume_token=SECRET_TOKEN&unknown=SECRET_BODY"
		c.Request = httptest.NewRequest(method, "/notify?"+secret, strings.NewReader(secret))
		rejectWebhookVerification(c, payment.TypeEasyPay, len(secret))
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Equal(t, "verify failed", w.Body.String())
	}
	require.Contains(t, logs.String(), "notification_rejected")
	require.Contains(t, logs.String(), "bodyLen=")
	require.NotContains(t, logs.String(), "SECRET")
	require.NotContains(t, logs.String(), "resume_token")
}

func TestWebhookBodyLimitsDoNotTruncateBeforeVerification(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, size := range []int{maxWebhookBodySize, maxWebhookBodySize + 1} {
			t.Run(fmt.Sprintf("%s/%d", method, size), func(t *testing.T) {
				payload := strings.Repeat("x", size)
				request := httptest.NewRequest(method, "/notify", strings.NewReader(payload))
				if method == http.MethodGet {
					request.URL.RawQuery = payload
				}
				body, err := readWebhookBody(request)
				if size == maxWebhookBodySize {
					require.NoError(t, err)
					require.Equal(t, payload, body)
				} else {
					require.Error(t, err)
					require.Empty(t, body)
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = request
					// An oversized body must be rejected before touching the service.
					c.Request.Body = io.NopCloser(strings.NewReader(payload))
					(&PaymentWebhookHandler{}).handleNotify(c, payment.TypeEasyPay)
					require.Equal(t, http.StatusBadRequest, w.Code)
				}
			})
		}
	}
}

func TestExtractOutTradeNo(t *testing.T) {
	tests := []struct {
		name        string
		providerKey string
		rawBody     string
		want        string
	}{
		{
			name:        "easypay query payload",
			providerKey: "easypay",
			rawBody:     "out_trade_no=sub2_123&trade_status=TRADE_SUCCESS",
			want:        "sub2_123",
		},
		{
			name:        "alipay query payload",
			providerKey: "alipay",
			rawBody:     "notify_time=2026-04-20+12%3A00%3A00&out_trade_no=sub2_456",
			want:        "sub2_456",
		},
		{
			name:        "unknown provider",
			providerKey: "wxpay",
			rawBody:     "{}",
			want:        "",
		},
		{
			name:        "airwallex payment intent payload",
			providerKey: payment.TypeAirwallex,
			rawBody:     `{"name":"payment_intent.succeeded","data":{"object":{"merchant_order_id":"sub2_awx_123"}}}`,
			want:        "sub2_awx_123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractOutTradeNo(tt.rawBody, tt.providerKey))
		})
	}
}

func TestVerifyNotificationWithProvidersReturnsMatchedProvider(t *testing.T) {
	firstErr := errors.New("wrong provider")
	providers := []payment.Provider{
		webhookHandlerProviderStub{
			key:       payment.TypeWxpay,
			verifyErr: firstErr,
		},
		webhookHandlerProviderStub{
			key: payment.TypeWxpay,
			notification: &payment.PaymentNotification{
				OrderID: "sub2_42",
				TradeNo: "trade-42",
				Status:  payment.NotificationStatusSuccess,
			},
		},
	}

	provider, notification, err := verifyNotificationWithProviders(context.Background(), providers, "{}", map[string]string{"wechatpay-signature": "sig"})
	require.NoError(t, err)
	require.Equal(t, payment.TypeWxpay, provider.ProviderKey())
	require.NotNil(t, notification)
	require.Equal(t, "sub2_42", notification.OrderID)
}

func TestVerifyNotificationWithProvidersFailsWhenAllProvidersReject(t *testing.T) {
	providers := []payment.Provider{
		webhookHandlerProviderStub{
			key:       payment.TypeWxpay,
			verifyErr: errors.New("verify failed a"),
		},
		webhookHandlerProviderStub{
			key:       payment.TypeWxpay,
			verifyErr: errors.New("verify failed b"),
		},
	}

	_, _, err := verifyNotificationWithProviders(context.Background(), providers, "{}", nil)
	require.Error(t, err)
}

type webhookHandlerProviderStub struct {
	key          string
	notification *payment.PaymentNotification
	verifyErr    error
}

type webhookHandlerServiceStub struct {
	lookupErr error
	handleErr error
	handled   bool
}

func (s *webhookHandlerServiceStub) GetWebhookProviders(context.Context, string, string) ([]payment.Provider, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	return []payment.Provider{webhookHandlerProviderStub{
		key:          payment.TypeEasyPay,
		notification: &payment.PaymentNotification{OrderID: "sub2_42", Status: payment.NotificationStatusSuccess},
	}}, nil
}

func (s *webhookHandlerServiceStub) HandlePaymentNotification(context.Context, *payment.PaymentNotification, string) error {
	s.handled = true
	return s.handleErr
}

func TestWebhookFailuresRemainRetryable(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		lookupErr, handleErr error
		wantStatus           int
		wantHandled          bool
	}{
		{"lookup outage", errors.New("database unavailable"), nil, http.StatusServiceUnavailable, false},
		{"provider missing", payment.ErrProviderNotFound, nil, http.StatusServiceUnavailable, false},
		{"fulfillment outage", nil, errors.New("database unavailable"), http.StatusInternalServerError, true},
		{"verified unknown order", nil, fmt.Errorf("wrapped: %w", service.ErrOrderNotFound), http.StatusOK, true},
		{"valid payment", nil, nil, http.StatusOK, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := &webhookHandlerServiceStub{lookupErr: tt.lookupErr, handleErr: tt.handleErr}
			h := &PaymentWebhookHandler{paymentService: stub}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader("out_trade_no=sub2_42"))
			h.handleNotify(c, payment.TypeEasyPay)
			require.Equal(t, tt.wantStatus, w.Code)
			require.Equal(t, tt.wantHandled, stub.handled)
		})
	}
}

func (p webhookHandlerProviderStub) Name() string        { return p.key }
func (p webhookHandlerProviderStub) ProviderKey() string { return p.key }
func (p webhookHandlerProviderStub) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.PaymentType(p.key)}
}
func (p webhookHandlerProviderStub) CreatePayment(context.Context, payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	panic("unexpected call")
}
func (p webhookHandlerProviderStub) QueryOrder(context.Context, string) (*payment.QueryOrderResponse, error) {
	panic("unexpected call")
}
func (p webhookHandlerProviderStub) VerifyNotification(context.Context, string, map[string]string) (*payment.PaymentNotification, error) {
	if p.verifyErr != nil {
		return nil, p.verifyErr
	}
	return p.notification, nil
}
func (p webhookHandlerProviderStub) Refund(context.Context, payment.RefundRequest) (*payment.RefundResponse, error) {
	panic("unexpected call")
}

func (s *webhookHandlerServiceStub) ConfirmNotificationUpstream(context.Context, payment.Provider, *payment.PaymentNotification) error {
	return nil
}
