package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/google/uuid"
)

// annotateOpenAIUpstreamError enriches a transport error returned by the
// common OpenAI error handler with the concrete request attempt metadata.
// Non-failover errors are returned unchanged.
func annotateOpenAIUpstreamError(req *http.Request, err error) error {
	if err == nil {
		return nil
	}
	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) {
		return annotateOpenAIAttemptFailover(req, failoverErr)
	}
	return err
}

// OpenAIUpstreamAttempt tracks one real outbound OpenAI request.  It is kept
// deliberately small and independent from the response/placeholder state so
// accounting can distinguish retries without changing response semantics.
type OpenAIUpstreamAttempt struct {
	ID        string
	StartedAt time.Time

	bodyWriteStarted    atomic.Bool
	bodyWriteCompleted  atomic.Bool
	responseHeadersSeen atomic.Bool
	realOutputStarted   atomic.Bool
	usageReceived       atomic.Bool
	statusCode          atomic.Int64
	upstreamRequestID   atomic.Value // string
}

func newOpenAIUpstreamAttempt() *OpenAIUpstreamAttempt {
	return &OpenAIUpstreamAttempt{ID: uuid.NewString(), StartedAt: time.Now()}
}

func newOpenAIUpstreamAttemptIDForAccount(account *Account) string {
	if account == nil || account.Platform != PlatformOpenAI {
		return ""
	}
	return newOpenAIUpstreamAttempt().ID
}

func withOpenAIUpstreamAttempt(ctx context.Context, attempt *OpenAIUpstreamAttempt) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if attempt == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.OpenAIAttemptID, attempt)
}

// OpenAIUpstreamAttemptFromContext returns the current attempt, if one was
// registered by an OpenAI forwarding path.
func OpenAIUpstreamAttemptFromContext(ctx context.Context) *OpenAIUpstreamAttempt {
	if ctx == nil {
		return nil
	}
	attempt, _ := ctx.Value(ctxkey.OpenAIAttemptID).(*OpenAIUpstreamAttempt)
	return attempt
}

func (a *OpenAIUpstreamAttempt) markBodyWriteStarted() {
	if a != nil {
		a.bodyWriteStarted.Store(true)
	}
}

func (a *OpenAIUpstreamAttempt) markBodyWriteCompleted() {
	if a != nil {
		a.bodyWriteCompleted.Store(true)
	}
}

// OpenAIUpstreamAttemptBodyStarted reports whether any request body bytes were
// read by net/http. A connection failure before this point is safe to retry.
func OpenAIUpstreamAttemptBodyStarted(ctx context.Context) bool {
	a := OpenAIUpstreamAttemptFromContext(ctx)
	return a != nil && a.bodyWriteStarted.Load()
}

func (a *OpenAIUpstreamAttempt) markResponse(respStatus int, requestID string) {
	if a == nil {
		return
	}
	a.responseHeadersSeen.Store(true)
	a.statusCode.Store(int64(respStatus))
	a.upstreamRequestID.Store(requestID)
}

func (a *OpenAIUpstreamAttempt) markUsageReceived() {
	if a != nil {
		a.usageReceived.Store(true)
	}
}

func (a *OpenAIUpstreamAttempt) markRealOutputStarted() {
	if a != nil {
		a.realOutputStarted.Store(true)
	}
}

// openAITrackedBody lets net/http tell us whether request bytes actually left
// the gateway. It does not buffer or transform the body.
type openAITrackedBody struct {
	reader  io.Reader
	attempt *OpenAIUpstreamAttempt
}

func (r *openAITrackedBody) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.attempt.markBodyWriteStarted()
	}
	if err == io.EOF {
		r.attempt.markBodyWriteCompleted()
	}
	return n, err
}

func (r *openAITrackedBody) Close() error {
	if closer, ok := r.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func trackOpenAIRequestBody(req *http.Request, ctx context.Context) {
	if req == nil || req.Body == nil {
		return
	}
	attempt := OpenAIUpstreamAttemptFromContext(ctx)
	if attempt == nil {
		return
	}
	req.Body = &openAITrackedBody{reader: req.Body, attempt: attempt}
}

// TrackOpenAIRequestBody applies attempt instrumentation to an outbound OpenAI
// request without changing its payload or Content-Length.
func TrackOpenAIRequestBody(req *http.Request, ctx context.Context) {
	trackOpenAIRequestBody(req, ctx)
}

func applyOpenAIStableClientRequestID(req *http.Request, ctx context.Context) {
	if req == nil || ctx == nil {
		return
	}
	clientRequestID, _ := ctx.Value(ctxkey.ClientRequestID).(string)
	clientRequestID = strings.TrimSpace(clientRequestID)
	if clientRequestID == "" {
		return
	}
	// Keep an explicit client value stable across all local retries while
	// avoiding forwarding arbitrary user-supplied header values.
	req.Header.Set("X-Client-Request-ID", "gateway-"+clientRequestID)
}

func openAIUnsettledAttemptResult(
	ctx context.Context,
	account *Account,
	model string,
	billingModel string,
	upstreamModel string,
	stream bool,
	duration time.Duration,
) *OpenAIForwardResult {
	if account == nil || account.Platform != PlatformOpenAI || !OpenAIUpstreamAttemptBodyStarted(ctx) {
		return nil
	}
	attempt := OpenAIUpstreamAttemptFromContext(ctx)
	if attempt == nil || strings.TrimSpace(attempt.ID) == "" {
		return nil
	}
	return &OpenAIForwardResult{
		AttemptID:                  strings.TrimSpace(attempt.ID),
		UpstreamRequestBodyStarted: true,
		Model:                      model,
		BillingModel:               billingModel,
		UpstreamModel:              upstreamModel,
		Stream:                     stream,
		Duration:                   duration,
	}
}
