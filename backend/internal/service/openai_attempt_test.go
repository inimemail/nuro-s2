package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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

func TestRecordUsageRequestIDUsesConcreteAttempt(t *testing.T) {
	attempt := newOpenAIUpstreamAttempt()
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "logical-request")
	result := &OpenAIForwardResult{RequestID: "upstream-request", AttemptID: attempt.ID}
	got := resolveOpenAIUsageBillingRequestID(ctx, result)
	require.Equal(t, "attempt:"+attempt.ID, got)
}

func TestApplyOpenAIStableClientRequestID(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "logical-request")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	require.NoError(t, err)
	applyOpenAIStableClientRequestID(req, ctx)
	require.Equal(t, "gateway-logical-request", req.Header.Get("X-Client-Request-ID"))
}

func TestConservativeOpenAIUnknownPricingCostIsNonZero(t *testing.T) {
	cost := conservativeOpenAIUnknownPricingCost(UsageTokens{InputTokens: 1_000, OutputTokens: 500}, 2)
	require.InDelta(t, 0.02, cost.InputCost, 1e-12)
	require.InDelta(t, 0.10, cost.OutputCost, 1e-12)
	require.InDelta(t, 0.12, cost.TotalCost, 1e-12)
	require.InDelta(t, 0.24, cost.ActualCost, 1e-12)
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

func TestOpenAIUnsettledAttemptResultRequiresOpenAIBodyWrite(t *testing.T) {
	attempt := newOpenAIUpstreamAttempt()
	ctx := withOpenAIUpstreamAttempt(context.Background(), attempt)
	account := &Account{Platform: PlatformOpenAI}
	require.Nil(t, openAIUnsettledAttemptResult(ctx, account, "gpt", "billing", "upstream", true, time.Second))

	attempt.markBodyWriteStarted()
	result := openAIUnsettledAttemptResult(ctx, account, "gpt", "billing", "upstream", true, time.Second)
	require.NotNil(t, result)
	require.Equal(t, attempt.ID, result.AttemptID)
	require.True(t, result.UpstreamRequestBodyStarted)
	require.Equal(t, "billing", result.BillingModel)

	require.Nil(t, openAIUnsettledAttemptResult(ctx, &Account{Platform: PlatformDeepSeek}, "deepseek", "", "", true, time.Second))
	require.Nil(t, openAIUnsettledAttemptResult(ctx, &Account{Platform: PlatformGrok}, "grok", "", "", true, time.Second))
}

func TestResolveOpenAIUsageBillingRequestIDPrefersAttemptID(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "client-request")
	result := &OpenAIForwardResult{RequestID: "upstream-request", AttemptID: "attempt-123"}
	require.Equal(t, "attempt:attempt-123", resolveOpenAIUsageBillingRequestID(ctx, result))

	result.AttemptID = ""
	require.Equal(t, "client:client-request", resolveOpenAIUsageBillingRequestID(ctx, result))
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
