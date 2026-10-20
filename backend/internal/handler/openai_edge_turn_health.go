package handler

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func openAIEdgeConfirmedAccountFailure(req service.OpenAIEdgeCompleteRequest) bool {
	if req.Success {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(req.FailureClass)) {
	case "upstream_disconnect", "upstream_no_progress":
		return !req.ClientDisconnected || req.TurnHealthReported
	case "upstream_error":
		return !req.ClientDisconnected || req.TurnHealthReported || req.UpstreamStatusCode >= http.StatusInternalServerError
	}
	return false
}

func (h *OpenAIGatewayHandler) openAIEdgeTurnHealth(c *gin.Context, req service.OpenAIEdgeCompleteRequest) {
	if len(req.HealthEventID) == 0 || len(req.HealthEventID) > 128 || h.gatewayService == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid turn health event"})
		return
	}
	lease := h.getOpenAIEdgeLease(req.LeaseID)
	if lease != nil {
		lease.retryMu.Lock()
		if lease.account == nil || lease.account.ID != req.AccountID || lease.edgeRequestID != req.EdgeRequestID {
			lease.retryMu.Unlock()
			c.JSON(http.StatusConflict, service.OpenAIEdgeAck{OK: false, Reason: "health_lease_mismatch"})
			return
		}
		if lease.settled {
			// Completion/expiry can take the lease after the map lookup but
			// before this lock. Authenticate the callback's snapshot just as
			// for an already-removed lease; a 409 would strand its WAL receipt.
			lease.retryMu.Unlock()
			lease = nil
		} else {
			defer lease.retryMu.Unlock()
		}
	}
	if lease == nil {
		var err error
		lease, err = h.restoreEdgeSettlement(req)
		if err != nil {
			c.JSON(http.StatusConflict, service.OpenAIEdgeAck{OK: false, Reason: "settlement_context_unavailable"})
			return
		}
	}
	if normalizeOpenAIEdgePath(lease.inboundEndpoint) != "/v1/responses:ws" || !strings.HasPrefix(req.HealthEventID, req.EdgeRequestID+":") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid turn health scope"})
		return
	}
	success := openAIEdgeCompletionIsSuccessful(lease.inboundEndpoint, req)
	neutral := req.CyberBlocked || openAIEdgeCachePolicyCompatibilityFailure(lease, req) || openAIEdgeCompletionIsRequestLocal(req) || openAIEdgeFailureClassIsLocalOrClient(req.FailureClass)
	terminal := strings.ToLower(req.TerminalEventType)
	neutral = neutral || ((terminal == "response.incomplete" || terminal == "response.cancelled" || terminal == "response.canceled") && !openAIEdgeFailureClassIsAccountTransport(req.FailureClass))
	neutral = neutral || (req.ClientDisconnected && !openAIEdgeConfirmedAccountFailure(req))
	if !neutral {
		model := strings.TrimSpace(req.Model)
		if model == "" {
			model = lease.openAIRoutingModel()
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 2*time.Second)
		defer cancel()
		eventID := req.EdgeRequestID + ":" + req.LeaseID + ":" + req.HealthEventID
		if err := h.gatewayService.RecordOpenAIStreamHealthEventWithContext(ctx, eventID, lease.account, model, req.FailureClass, success, openAIEdgeRealFirstTokenMS(req.FirstTokenMS, req.RealFirstTokenMS), req.UpstreamStatusCode, req.ErrorMessage); err != nil {
			c.JSON(http.StatusServiceUnavailable, service.OpenAIEdgeAck{OK: false, Reason: "health_receipt_unavailable"})
			return
		}
	}
	c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true})
}
