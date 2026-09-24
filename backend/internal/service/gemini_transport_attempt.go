package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
)

// Only an observed pre-send dial/DNS failure is safe to hand to the scheduler.
// Timeouts/EOF after a possibly transmitted POST never trigger a second request.
func trackGeminiTransportAttempt(req *http.Request) (*http.Request, *atomic.Bool) {
	sent := &atomic.Bool{}
	trace := &httptrace.ClientTrace{WroteHeaders: func() { sent.Store(true) }, WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) }}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	req.GetBody = nil
	return req, sent
}
func geminiTransportCanFailover(ctx context.Context, sent *atomic.Bool, err error) bool {
	if ctx.Err() != nil || sent == nil || sent.Load() || errors.Is(err, context.Canceled) {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return op.Op == "dial"
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}
