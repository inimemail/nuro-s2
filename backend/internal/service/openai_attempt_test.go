package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestOpenAITrackedBodyMarksOnlyWhenBytesAreRead(t *testing.T) {
	attempt := newOpenAIUpstreamAttempt()
	ctx := withOpenAIUpstreamAttempt(context.Background(), attempt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", strings.NewReader("payload"))
	require.NoError(t, err)
	trackOpenAIRequestBody(req, ctx)
	require.False(t, OpenAIUpstreamAttemptBodyStarted(ctx))

	buf := make([]byte, 16)
	_, err = req.Body.Read(buf)
	require.NoError(t, err)
	require.True(t, OpenAIUpstreamAttemptBodyStarted(ctx))
	_, err = req.Body.Read(buf)
	require.ErrorIs(t, err, io.EOF)
}

func TestApplyOpenAIStableClientRequestID(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "logical-request")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	require.NoError(t, err)
	applyOpenAIStableClientRequestID(req, ctx)
	require.Equal(t, "gateway-logical-request", req.Header.Get("X-Client-Request-ID"))
}

func TestOpenAIAttemptIDDoesNotChangeLogicalBillingRequestID(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "logical-request")
	result := &OpenAIForwardResult{RequestID: "upstream-request", AttemptID: "attempt-123"}
	require.Equal(t, "client:logical-request", resolveUsageBillingRequestID(ctx, result.RequestID))
}

func TestOpenAIWebSocketTurnsUseIndependentBillingRequestIDs(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "logical-request")
	first := &OpenAIForwardResult{OpenAIWSMode: true, RequestID: "req-1", ResponseID: "resp-1"}
	second := &OpenAIForwardResult{OpenAIWSMode: true, RequestID: "req-2", ResponseID: "resp-2"}
	require.Equal(t, "ws-turn:resp-1", resolveOpenAIUsageBillingRequestID(ctx, first))
	require.Equal(t, "ws-turn:resp-2", resolveOpenAIUsageBillingRequestID(ctx, second))
	require.Equal(t, "ws-turn:resp-1", resolveOpenAIUsageBillingRequestID(ctx, first))
}

func TestOpenAIWebSocketBillingRequestIDFallsBackToRequestID(t *testing.T) {
	result := &OpenAIForwardResult{OpenAIWSMode: true, RequestID: "req-1"}
	require.Equal(t, "ws-turn:req-1", resolveOpenAIUsageBillingRequestID(context.Background(), result))
}

func TestAnnotateOpenAIAttemptFailoverStopsSameAccountReplay(t *testing.T) {
	attempt := newOpenAIUpstreamAttempt()
	attempt.markBodyWriteStarted()
	ctx := withOpenAIUpstreamAttempt(context.Background(), attempt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/v1/responses", nil)
	require.NoError(t, err)

	failoverErr := annotateOpenAIAttemptFailover(req, &UpstreamFailoverError{RetryableOnSameAccount: true})
	require.Equal(t, attempt.ID, failoverErr.AttemptID)
	require.True(t, failoverErr.UpstreamRequestBodyStarted)
	require.True(t, failoverErr.ExecutionUnknown)
	require.False(t, failoverErr.RetryableOnSameAccount)
}

func TestAnnotateOpenAIAttemptFailoverExplicitClientErrorIsKnown(t *testing.T) {
	attempt := newOpenAIUpstreamAttempt()
	attempt.markBodyWriteStarted()
	ctx := withOpenAIUpstreamAttempt(context.Background(), attempt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/v1/responses", nil)
	require.NoError(t, err)

	failoverErr := annotateOpenAIAttemptFailover(req, &UpstreamFailoverError{StatusCode: http.StatusBadRequest, RetryableOnSameAccount: true})
	require.False(t, failoverErr.ExecutionUnknown)
	// A concrete 4xx rejection is a known outcome; preserve the caller's
	// established compatibility retry rule. Only transport/5xx unknown
	// execution disables same-account replay.
	require.True(t, failoverErr.RetryableOnSameAccount)
}

func TestOpenAIStreamFailureAfterRequestWriteStopsReplay(t *testing.T) {
	attempt := newOpenAIUpstreamAttempt()
	attempt.markBodyWriteStarted()
	ctx := withOpenAIUpstreamAttempt(context.Background(), attempt)

	failoverErr := (&OpenAIGatewayService{}).newOpenAIStreamFailoverErrorWithContext(
		ctx,
		nil,
		&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		false,
		"request-stream",
		nil,
		"stream ended before a terminal event",
	)

	require.Equal(t, attempt.ID, failoverErr.AttemptID)
	require.True(t, failoverErr.UpstreamRequestBodyStarted)
	require.True(t, failoverErr.ExecutionUnknown)
	require.False(t, failoverErr.RetryableOnSameAccount)
}

func TestOpenAIStreamExplicitRateLimitAfterRequestWriteRemainsKnown(t *testing.T) {
	attempt := newOpenAIUpstreamAttempt()
	attempt.markBodyWriteStarted()
	ctx := withOpenAIUpstreamAttempt(context.Background(), attempt)

	failoverErr := (&OpenAIGatewayService{}).newOpenAIStreamFailoverErrorWithContext(
		ctx,
		nil,
		&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		false,
		"request-stream",
		[]byte(`{"response":{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}}`),
		"rate limited",
	)

	require.True(t, failoverErr.UpstreamRequestBodyStarted)
	require.False(t, failoverErr.ExecutionUnknown)
}

func TestOpenAIStreamFailureTrackingDoesNotAffectOtherPlatforms(t *testing.T) {
	attempt := newOpenAIUpstreamAttempt()
	attempt.markBodyWriteStarted()
	ctx := withOpenAIUpstreamAttempt(context.Background(), attempt)

	failoverErr := (&OpenAIGatewayService{}).newOpenAIStreamFailoverErrorWithContext(
		ctx,
		nil,
		&Account{Platform: PlatformGrok, Type: AccountTypeAPIKey},
		false,
		"request-grok-stream",
		nil,
		"stream ended before a terminal event",
	)

	require.Empty(t, failoverErr.AttemptID)
	require.False(t, failoverErr.UpstreamRequestBodyStarted)
	require.False(t, failoverErr.ExecutionUnknown)
}

func TestOpenAITrackedBodyPreservesRequestContentLength(t *testing.T) {
	attempt := newOpenAIUpstreamAttempt()
	ctx := withOpenAIUpstreamAttempt(context.Background(), attempt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", strings.NewReader("payload"))
	require.NoError(t, err)
	want := req.ContentLength

	trackOpenAIRequestBody(req, ctx)
	require.Equal(t, want, req.ContentLength)
}

func TestOpenAIWSAttemptIDIsScopedToOpenAIPlatform(t *testing.T) {
	require.NotEmpty(t, newOpenAIUpstreamAttemptIDForAccount(&Account{Platform: PlatformOpenAI}))
	require.Empty(t, newOpenAIUpstreamAttemptIDForAccount(&Account{Platform: PlatformGrok}))
	require.Empty(t, newOpenAIUpstreamAttemptIDForAccount(&Account{Platform: PlatformDeepSeek}))
}

func TestOpenAIImagesApplicationErrorPreservesAttemptAndDisablesReplay(t *testing.T) {
	err := &OpenAIImagesUpstreamError{
		StatusCode:                 http.StatusBadGateway,
		Message:                    "incomplete",
		AttemptID:                  "attempt-image",
		UpstreamRequestBodyStarted: true,
	}
	failover := err.ToFailoverErrorWithModelLimitProtection(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, true)
	require.Equal(t, "attempt-image", failover.AttemptID)
	require.True(t, failover.UpstreamRequestBodyStarted)
	require.True(t, failover.ExecutionUnknown)
	require.False(t, failover.RetryableOnSameAccount)
}

func TestOpenAIImagesApplicationErrorDoesNotBlockOtherPlatformReplay(t *testing.T) {
	err := &OpenAIImagesUpstreamError{
		StatusCode:                 http.StatusBadGateway,
		Message:                    "incomplete",
		AttemptID:                  "attempt-grok-image",
		UpstreamRequestBodyStarted: true,
	}
	failover := err.ToFailoverErrorWithModelLimitProtection(&Account{Platform: PlatformGrok, Type: AccountTypeAPIKey}, true)
	require.False(t, failover.ExecutionUnknown)
}
