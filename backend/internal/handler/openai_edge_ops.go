package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/tidwall/gjson"
)

const edgeOpsStreamFailureKey = "edge_ops_stream_failure"

func (lease *openAIEdgeLease) clearFailureDiagnostic() {
	lease.lastFailureDiagnostic = ""
	lease.lastFailureStatus = 0
	lease.lastFailureRequestID = ""
	lease.lastFailureResponseID = ""
}

// The public ingress sees only sanitized stream bytes. Control-plane callbacks
// own the account/model attribution, including deployments entering Edge directly.
// Keep this best-effort observation separate from billing and retry decisions.
func (h *OpenAIGatewayHandler) recordOpenAIEdgeFailure(lease *openAIEdgeLease, stage, kind, detail, upstreamID, responseID string, upstreamStatus int) {
	if h == nil || h.opsService == nil || lease == nil || lease.account == nil || lease.apiKey == nil || !h.opsService.IsMonitoringEnabled(context.Background()) {
		return
	}
	key := lease.edgeRequestID + ":" + lease.leaseID
	message := "Edge request failed"
	failureClass := kind
	if stage == "abort" {
		if lease.lastFailureDiagnostic != "" {
			detail = lease.lastFailureDiagnostic
		}
		upstreamStatus = lease.lastFailureStatus
		upstreamID = lease.lastFailureRequestID
		responseID = lease.lastFailureResponseID
	}
	kind = edgeOpsErrorType(kind)
	// The internal diagnostic contains only bounded structured fields. Legacy
	// raw errors are sanitized and never copied into the public response.
	diagnostic := map[string]any{"stage": stage, "edge_request_id": lease.edgeRequestID, "response_id": boundedEdgeDiagnosticID(responseID)}
	var supplied map[string]json.RawMessage
	if len(detail) <= 8192 && json.Unmarshal([]byte(detail), &supplied) == nil {
		for _, name := range []string{"event_type", "error_type", "code", "status", "request_id"} {
			var value string
			if json.Unmarshal(supplied[name], &value) == nil {
				if value = boundedEdgeDiagnosticID(value); value != "" {
					diagnostic[name] = value
				}
			}
		}
	} else {
		if len(detail) > 8192 {
			diagnostic["reason"] = "Edge failure detail exceeded diagnostic limit"
		} else {
			diagnostic["reason"] = service.SanitizeUpstreamErrorMessageForClient(detail)
		}
		if prefix, _, ok := strings.Cut(detail, ":"); ok {
			if reason := boundedEdgeDiagnosticID(strings.TrimSpace(prefix)); strings.HasPrefix(reason, "edge_") || strings.HasPrefix(reason, "early_placeholder_") || strings.HasPrefix(reason, "retry_") {
				diagnostic["reason_code"] = reason
			}
		}
	}
	body, _ := json.Marshal(diagnostic)
	if shouldSkipOpsErrorLog(context.Background(), h.opsService, message, string(body), lease.inboundEndpoint) || key == ":" || !h.claimEdgeOpsFailure(key) {
		return
	}
	if errorType, ok := diagnostic["error_type"].(string); ok {
		kind = edgeOpsErrorType(errorType)
	}
	if kind == "upstream_error" {
		switch diagnostic["code"] {
		case "rate_limit_exceeded", "insufficient_quota":
			kind = "rate_limit_error"
		case "invalid_api_key":
			kind = "authentication_error"
		case "permission_denied", "cyber_policy":
			kind = "forbidden_error"
		case "model_not_found", "context_length_exceeded", "invalid_request_error":
			kind = "invalid_request_error"
		}
	}
	if kind == "upstream_error" {
		switch upstreamStatus {
		case http.StatusTooManyRequests:
			kind = "rate_limit_error"
		case http.StatusUnauthorized:
			kind = "authentication_error"
		case http.StatusForbidden:
			kind = "forbidden_error"
		case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
			kind = "invalid_request_error"
		}
	}
	accountID, apiKeyID := lease.account.ID, lease.apiKey.ID
	status := upstreamStatus
	if status < http.StatusBadRequest {
		// Ops' default error query excludes status < 400. Record the failure
		// status here, retaining the actual HTTP status in UpstreamStatusCode.
		status = edgeOpsFailureStatus(kind)
	}
	phase := "upstream"
	if upstreamStatus == 0 && diagnostic["event_type"] == nil && diagnostic["code"] == nil {
		phase = "network"
		switch failureClass {
		case "local_capacity_rejected", "queue_timeout":
			phase = "routing"
		case "lease_lost", "prepare_failed", "complete_failed":
			phase = "internal"
		}
	}
	entry := &service.OpsInsertErrorLogInput{
		RequestID: lease.edgeRequestID, ClientRequestID: lease.edgeRequestID,
		AccountID: &accountID, APIKeyID: &apiKeyID, GroupID: lease.apiKey.GroupID,
		APIKeyPrefix: keyPrefix(lease.apiKey.Key, 8), Platform: lease.account.Platform,
		Model: lease.requestModel, RequestedModel: lease.requestModel, UpstreamModel: lease.upstreamModel,
		RequestPath: lease.inboundEndpoint, InboundEndpoint: lease.inboundEndpoint, UpstreamEndpoint: lease.upstreamEndpoint,
		Stream: true, UserAgent: lease.userAgent, ErrorPhase: phase, ErrorType: kind,
		Severity: classifyOpsSeverity(kind, status), StatusCode: status, ErrorMessage: message,
		ErrorSource: classifyOpsErrorSource(phase, message), ErrorOwner: classifyOpsErrorOwner(phase, message),
		CreatedAt: time.Now(),
		UpstreamErrors: []*service.OpsUpstreamErrorEvent{{
			AtUnixMs: time.Now().UnixMilli(), Stage: stage, Kind: "edge_failure",
			Platform: lease.account.Platform, AccountID: accountID, AccountName: lease.account.Name,
			UpstreamStatusCode: upstreamStatus, UpstreamRequestID: boundedEdgeDiagnosticID(upstreamID),
			Message: message, Detail: string(body),
		}},
	}
	if lease.apiKey.User != nil {
		id := lease.apiKey.User.ID
		entry.UserID = &id
	}
	if upstreamStatus > 0 {
		entry.UpstreamStatusCode = &upstreamStatus
	}
	enqueueOpsErrorLog(h.opsService, entry)
}

func edgeOpsFailureStatus(kind string) int {
	switch kind {
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "invalid_request_error":
		return http.StatusBadRequest
	case "authentication_error":
		return http.StatusUnauthorized
	case "forbidden_error":
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}

func edgeOpsErrorType(kind string) string {
	if kind == "permission_error" || kind == "safety_error" {
		return "forbidden_error"
	}
	if isKnownOpsErrorType(kind) {
		return kind
	}
	return "upstream_error"
}

func edgeOpsDiagnosticFromPayload(payload []byte) string {
	if !gjson.ValidBytes(payload) {
		return ""
	}
	fields := map[string]string{}
	for name, paths := range map[string][]string{
		"event_type": {"type"}, "error_type": {"response.error.type", "error.type"},
		"code": {"response.error.code", "error.code"}, "status": {"response.error.status", "error.status"},
		"request_id": {"response.error.request_id", "error.request_id"},
	} {
		for _, path := range paths {
			if value := boundedEdgeDiagnosticID(gjson.GetBytes(payload, path).String()); value != "" {
				fields[name] = value
				break
			}
		}
	}
	if len(fields) == 0 {
		return ""
	}
	data, _ := json.Marshal(fields)
	return string(data)
}

func boundedEdgeDiagnosticID(value string) string {
	if len(value) > 128 {
		return ""
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-.:", c)) {
			return ""
		}
	}
	return value
}

func (h *OpenAIGatewayHandler) claimEdgeOpsFailure(key string) bool {
	// Redis deduplicates replayed completion capsules across Go replicas. The
	// bounded local fallback avoids making Redis health a billing dependency.
	if h.redisClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		sum := sha256.Sum256([]byte(key))
		// The shared client does not enable ContextTimeoutEnabled. A context
		// alone cannot cap socket I/O; clone timeout options without changing
		// the shared pool/client used by billing, leases and scheduling.
		ok, err := h.redisClient.WithTimeout(100*time.Millisecond).SetNX(ctx, "edge:ops:failure:"+hex.EncodeToString(sum[:]), "1", 24*time.Hour).Result()
		cancel()
		if err == nil {
			return ok
		}
	}
	h.edgeOpsMu.Lock()
	defer h.edgeOpsMu.Unlock()
	now := time.Now()
	if seen, ok := h.edgeOpsSeen[key]; ok && now.Sub(seen) < 24*time.Hour {
		return false
	}
	if h.edgeOpsSeen == nil {
		h.edgeOpsSeen = make(map[string]time.Time)
	}
	if len(h.edgeOpsSeen) >= 4096 {
		var oldestKey string
		oldest := now
		for k, at := range h.edgeOpsSeen {
			if at.Before(oldest) {
				oldestKey, oldest = k, at
			}
		}
		delete(h.edgeOpsSeen, oldestKey)
	}
	h.edgeOpsSeen[key] = now
	return true
}
