package service

import (
	"encoding/base64"
	"encoding/json"
)

const (
	OpenAIEdgeActionFallbackGo   = "fallback_go"
	OpenAIEdgeActionRelay        = "relay"
	OpenAIEdgeActionRespondError = "respond_error"

	OpenAIEdgeTransportHTTP2SSE = "http2_sse"
	OpenAIEdgeTransportWSV2     = "ws_v2"

	OpenAIEdgeDialectChatCompletions = "chat_completions"
	OpenAIEdgeDialectResponses       = "responses"
)

// OpenAIEdgePrepareRequest is sent by the Rust data plane to the Go control
// plane before it commits any client response. Go remains authoritative for
// auth, billing, scheduling, soft cooling, pool mode, sticky routing, and
// request transformation.
type OpenAIEdgePrepareRequest struct {
	EdgeRequestID string            `json:"edge_request_id"`
	Method        string            `json:"method"`
	Path          string            `json:"path"`
	RawQuery      string            `json:"raw_query,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Body          json.RawMessage   `json:"body,omitempty"`
	BodyRawBase64 string            `json:"body_raw_base64,omitempty"`
	ClientIP      string            `json:"client_ip,omitempty"`
	Stream        *bool             `json:"stream,omitempty"`
}

type OpenAIEdgePlan struct {
	Action          string            `json:"action"`
	Reason          string            `json:"reason,omitempty"`
	EdgeRequestID   string            `json:"edge_request_id"`
	LeaseID         string            `json:"lease_id,omitempty"`
	LeaseTTLMS      int               `json:"lease_ttl_ms,omitempty"`
	AccountID       int64             `json:"account_id,omitempty"`
	AccountType     string            `json:"account_type,omitempty"`
	Transport       string            `json:"transport,omitempty"`
	ResponseDialect string            `json:"response_dialect,omitempty"`
	UpstreamURL     string            `json:"upstream_url,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            json.RawMessage   `json:"body,omitempty"`
	BodyRawBase64   string            `json:"body_raw_base64,omitempty"`
	ProxyURL        string            `json:"proxy_url,omitempty"`
	LowLatencyMode  string            `json:"low_latency_mode,omitempty"`
	Lane            string            `json:"lane,omitempty"`
	// SafeTokenPlaceholder lets edge-rs mirror the Go Responses SSE behavior:
	// after response.created, inject an empty output_text.delta so compatible
	// downstream panels can record an early first token without visible text.
	SafeTokenPlaceholder bool `json:"safe_token_placeholder,omitempty"`
	// FirstTokenTimeoutPlaceholderMS injects an empty placeholder after the
	// configured timeout when upstream has not produced a real first token.
	// It must not be reported as first_token_ms.
	FirstTokenTimeoutPlaceholderMS int `json:"first_token_timeout_placeholder_ms,omitempty"`
}

type OpenAIEdgeRetryRequest struct {
	EdgeRequestID       string          `json:"edge_request_id"`
	LeaseID             string          `json:"lease_id,omitempty"`
	AccountID           int64           `json:"account_id,omitempty"`
	UpstreamStatusCode  int             `json:"upstream_status_code,omitempty"`
	UpstreamRequestID   string          `json:"upstream_request_id,omitempty"`
	ErrorType           string          `json:"error_type,omitempty"`
	ErrorMessage        string          `json:"error_message,omitempty"`
	ResponseBody        json.RawMessage `json:"response_body,omitempty"`
	WroteClientResponse bool            `json:"wrote_client_response"`
}

type OpenAIEdgeRetryDecision struct {
	Action          string          `json:"action"`
	Reason          string          `json:"reason,omitempty"`
	Plan            *OpenAIEdgePlan `json:"plan,omitempty"`
	FailureRecorded bool            `json:"failure_recorded,omitempty"`
	StatusCode      int             `json:"status_code,omitempty"`
	ErrorType       string          `json:"error_type,omitempty"`
	ErrorMessage    string          `json:"error_message,omitempty"`
}

type OpenAIEdgeCompleteRequest struct {
	EdgeRequestID       string      `json:"edge_request_id"`
	LeaseID             string      `json:"lease_id,omitempty"`
	AccountID           int64       `json:"account_id,omitempty"`
	Success             bool        `json:"success"`
	ClientDisconnected  bool        `json:"client_disconnected,omitempty"`
	RequestID           string      `json:"request_id,omitempty"`
	ResponseID          string      `json:"response_id,omitempty"`
	Model               string      `json:"model,omitempty"`
	UpstreamModel       string      `json:"upstream_model,omitempty"`
	Usage               OpenAIUsage `json:"usage,omitempty"`
	DurationMS          int64       `json:"duration_ms,omitempty"`
	UpstreamHeaderMS    *int64      `json:"upstream_header_ms,omitempty"`
	UpstreamFirstByteMS *int64      `json:"upstream_first_byte_ms,omitempty"`
	FirstTokenMS        *int64      `json:"first_token_ms,omitempty"`
	RealFirstTokenMS    *int64      `json:"real_first_token_ms,omitempty"`
	FirstClientFlushMS  *int64      `json:"first_client_flush_ms,omitempty"`
	EdgePrepareMS       *int64      `json:"edge_prepare_ms,omitempty"`
	EdgeQueueWaitMS     *int64      `json:"edge_queue_wait_ms,omitempty"`
	EdgeRelayStartMS    *int64      `json:"edge_relay_start_ms,omitempty"`
	EdgeFallbackReason  string      `json:"edge_fallback_reason,omitempty"`
	EdgeRetryCount      *int64      `json:"edge_retry_count,omitempty"`
	ErrorType           string      `json:"error_type,omitempty"`
	ErrorMessage        string      `json:"error_message,omitempty"`
	UpstreamStatusCode  int         `json:"upstream_status_code,omitempty"`
	TerminalEventType   string      `json:"terminal_event_type,omitempty"`
	CyberBlocked        bool        `json:"cyber_blocked,omitempty"`
}

type OpenAIEdgeAbortRequest struct {
	EdgeRequestID      string `json:"edge_request_id"`
	LeaseID            string `json:"lease_id,omitempty"`
	AccountID          int64  `json:"account_id,omitempty"`
	Reason             string `json:"reason,omitempty"`
	ClientDisconnected bool   `json:"client_disconnected,omitempty"`
}

type OpenAIEdgeAck struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

func EncodeOpenAIEdgeRawBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(body)
}
