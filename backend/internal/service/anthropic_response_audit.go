package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"log/slog"
)

// Diagnostic-only audit, emitted after forwarding; observed tier never replaces
// the requested billing tier or downstream usage presentation.
func auditAnthropicResponseTier(ctx context.Context, c *gin.Context, account *Account) {
	if c == nil || account == nil {
		return
	}
	o := upstreamResponseModelObserverFromContext(c)
	if o == nil {
		return
	}
	o.mu.RLock()
	tier := o.anthropicServiceTier
	o.mu.RUnlock()
	if tier == "" {
		return
	}
	if sent, _ := c.Get("anthropic_response_tier_audited"); sent == true {
		return
	}
	c.Set("anthropic_response_tier_audited", true)
	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	slog.Info("anthropic_upstream_response_audit", "request_id", normalizeObservedUpstreamResponseModel(requestID), "account_id", account.ID, "upstream_endpoint", "/v1/messages", "response_model", observedUpstreamResponseModel(c), "observed_service_tier", tier)
}
