package service

import (
	"context"
	"errors"
	"time"

	"github.com/gin-gonic/gin"
)

// A provider read deadline is not a downstream cancellation. Keep a distinct
// error so health accounting does not discard it as context.DeadlineExceeded.
type openAIWSUpstreamTimeoutError struct{ noProgress bool }

func (e *openAIWSUpstreamTimeoutError) Error() string {
	if e.noProgress {
		return "upstream websocket made no effective progress within the configured body idle window"
	}
	return "upstream websocket read timeout"
}

type openAIWSProgressGuard struct {
	timeout   time.Duration
	remaining time.Duration
}

func (s *OpenAIGatewayService) newOpenAIWSProgressGuard(c *gin.Context) *openAIWSProgressGuard {
	guard := &openAIWSProgressGuard{}
	if s == nil || s.cfg == nil {
		return guard
	}
	profile := s.cfg.GatewayEdgeProtection()
	if !profile.EdgeProtectionEnabled || profile.EdgeBodyIdleTimeoutMS <= 0 {
		return guard
	}
	if c != nil {
		if value, ok := c.Get("api_key"); ok {
			if key, ok := value.(*APIKey); ok && key != nil && key.Group != nil && key.Group.EdgeProtectionEnabled != nil && !*key.Group.EdgeProtectionEnabled {
				return guard
			}
		}
	}
	guard.timeout = time.Duration(profile.EdgeBodyIdleTimeoutMS) * time.Millisecond
	guard.remaining = guard.timeout
	return guard
}
func (g *openAIWSProgressGuard) readTimeout(legacy time.Duration) (time.Duration, bool) {
	if g.timeout <= 0 {
		return legacy, false
	}
	if g.remaining <= 0 {
		return time.Nanosecond, true
	}
	if legacy <= 0 || g.remaining <= legacy {
		return g.remaining, true
	}
	return legacy, false
}

// Consume only upstream read time. A slow downstream writer or local event
// processing is not evidence that the upstream account is stuck.
func (g *openAIWSProgressGuard) consumedRead(elapsed time.Duration) {
	g.remaining -= elapsed
}
func (g *openAIWSProgressGuard) observe(payload []byte, eventType string) {
	if classifyOpenAIStreamProgress(payload, eventType).semantic {
		g.remaining = g.timeout
	}
}
func openAIWSReadError(ctx context.Context, err error, noProgress bool) error {
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return &openAIWSUpstreamTimeoutError{noProgress: noProgress}
	}
	return err
}
func (s *OpenAIGatewayService) recordOpenAIWSTimeout(account *Account, err error) bool {
	var timeout *openAIWSUpstreamTimeoutError
	if !errors.As(err, &timeout) {
		return false
	}
	if timeout.noProgress {
		s.blockOpenAIAccountTransportFailure(account, "upstream_no_progress", "upstream response made no effective progress")
	} else {
		s.recordOpenAIProxyStreamOutcome(account, OpenAIStreamFailureUpstreamDisconnect, false, false, err)
	}
	return true
}
func (s *OpenAIGatewayService) RecordOpenAIAccountWSFailure(account *Account, model string, err error) {
	if !s.recordOpenAIWSTimeout(account, err) {
		s.recordOpenAIProxyStreamOutcome(account, OpenAIStreamFailureUpstreamDisconnect, true, false, err)
	}
	s.ReportOpenAIAccountScheduleResultForRequest(account, model, false, nil)
}
