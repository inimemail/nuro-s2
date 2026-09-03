package service

import (
	"encoding/base64"
	"encoding/json"
	"time"
)

const (
	OpenAIEdgeActionFallbackGo   = "fallback_go"
	OpenAIEdgeActionRelay        = "relay"
	OpenAIEdgeActionKeepCurrent  = "keep_current"
	OpenAIEdgeActionRespondError = "respond_error"

	OpenAIEdgeCommitStateNone        = "none"
	OpenAIEdgeCommitStateGatewayOnly = "gateway_only"
	OpenAIEdgeCommitStateRealOutput  = "real_output"
	OpenAIEdgeCommitStateTerminal    = "terminal"

	OpenAIEdgeRetryStageActivate = "activate"
	OpenAIEdgeRetryStageCancel   = "cancel"

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
	EdgeRequestID  string            `json:"edge_request_id"`
	EdgeNodeID     string            `json:"edge_node_id,omitempty"`
	EdgeInstanceID string            `json:"edge_instance_id,omitempty"`
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	RawQuery       string            `json:"raw_query,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           json.RawMessage   `json:"body,omitempty"`
	BodyRawBase64  string            `json:"body_raw_base64,omitempty"`
	ClientIP       string            `json:"client_ip,omitempty"`
	Stream         *bool             `json:"stream,omitempty"`
	// PreferredAccountID is an untrusted Rust route-cache hint. Go must fully
	// revalidate it and may ignore it before issuing any lease.
	PreferredAccountID int64 `json:"preferred_account_id,omitempty"`
}

const OpenAIEdgeControlProtocolVersion = 2

type OpenAIEdgeControlSnapshot struct {
	ProtocolVersion int       `json:"protocol_version"`
	Generation      int64     `json:"generation"`
	GeneratedAt     time.Time `json:"generated_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	ExpiresAtUnixMS int64     `json:"expires_at_unix_ms"`
	Enabled         bool      `json:"enabled"`
	Ready           bool      `json:"ready"`
	Reason          string    `json:"reason,omitempty"`
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
	// after response.created, inject a non-content transport_progress.delta so
	// compatible downstream panels can record an early first token without
	// changing answer text, reasoning, tool state, or usage.
	SafeTokenPlaceholder bool `json:"safe_token_placeholder,omitempty"`
	// FirstTokenTimeoutPlaceholderMS injects the same non-content progress event
	// after the configured timeout when downstream has not observed a countable
	// delta. It must not be reported as first_token_ms or real_first_token_ms.
	FirstTokenTimeoutPlaceholderMS int `json:"first_token_timeout_placeholder_ms,omitempty"`
	// SemanticProgressTimeoutMS is learned from successful, content-free stream
	// timing samples. It is not an administrator setting and is omitted while
	// the route is still learning or temporarily suspended.
	SemanticProgressTimeoutMS int `json:"semantic_progress_timeout_ms,omitempty"`
	// RaceResponseHeaderTimeoutMS bounds only the response-header wait while the
	// request-level Edge race budget is active, including switched accounts. A
	// successful SSE body is never time-limited by it.
	RaceResponseHeaderTimeoutMS int `json:"race_response_header_timeout_ms,omitempty"`
	// Edge upstream protection is request-scoped so admin changes can reach a
	// long-running Edge process without rebuilding its HTTP client pool.
	EdgeProtectionEnabled         bool `json:"edge_protection_enabled"`
	EdgeConnectTimeoutMS          int  `json:"edge_connect_timeout_ms,omitempty"`
	EdgeResponseHeaderTimeoutMS   int  `json:"edge_response_header_timeout_ms,omitempty"`
	EdgeResponseHeaderBudgetMS    int  `json:"edge_response_header_budget_ms,omitempty"`
	EdgeBodyIdleTimeoutMS         int  `json:"edge_body_idle_timeout_ms,omitempty"`
	EdgeResponseHeaderMaxAttempts int  `json:"edge_response_header_max_attempts,omitempty"`
	EdgeResponseHeaderFailover    bool `json:"edge_response_header_failover"`
	// ExposeRetriedUsage controls the downstream usage frame only. Go still
	// receives the complete provider usage through the completion callback.
	ExposeRetriedUsage bool `json:"expose_retried_usage"`
	// Go-only group override; the final EdgeProtectionEnabled value is sent on wire.
	EdgeProtectionGroupEnabled *bool `json:"-"`
	// SSECommentPreflush mirrors the account-level APIKey/OAuth setting that
	// sends an SSE comment before upstream data so the downstream can commit the
	// response body earlier. It is deliberately optional for old edge binaries.
	SSECommentPreflush bool `json:"sse_comment_preflush,omitempty"`
	// PreambleFlush controls whether Responses preamble events
	// (response.created/response.in_progress) are sent before a real output
	// event. It is separate from SSECommentPreflush, which emits a local SSE
	// comment instead of forwarding an upstream event.
	// Keep the bool on the wire even when disabled. Rust edge-rs defaults a
	// missing field to the legacy immediate-flush behavior for older control
	// planes, so an explicit false is required to enable preamble buffering.
	PreambleFlush bool `json:"preamble_flush"`
	// PromptCacheCreationOptimizationMode carries the account policy for edge-rs
	// WS turns. Applied reports whether the current outgoing body was rewritten.
	PromptCacheCreationOptimizationMode    string                             `json:"prompt_cache_creation_optimization_mode,omitempty"`
	PromptCacheCreationOptimizationModel   string                             `json:"prompt_cache_creation_optimization_model,omitempty"`
	PromptCacheCreationOptimizationApplied bool                               `json:"prompt_cache_creation_optimization_applied,omitempty"`
	DownstreamCacheUsageMode               string                             `json:"downstream_cache_usage_mode,omitempty"`
	DownstreamCacheUsageModel              string                             `json:"downstream_cache_usage_model,omitempty"`
	DownstreamCacheMarkup                  *OpenAIDownstreamCacheMarkupPolicy `json:"downstream_cache_markup,omitempty"`
	DownstreamCacheMarkupModel             string                             `json:"downstream_cache_markup_model,omitempty"`
	MaxReasoningEffort                     string                             `json:"max_reasoning_effort,omitempty"`
	MaxReasoningEffortOverLimit            string                             `json:"max_reasoning_effort_over_limit,omitempty"`
	ReasoningEffortMappings                []ReasoningEffortMapping           `json:"reasoning_effort_mappings,omitempty"`
}

type OpenAIEdgeRetryRequest struct {
	EdgeRequestID      string          `json:"edge_request_id"`
	LeaseID            string          `json:"lease_id,omitempty"`
	AccountID          int64           `json:"account_id,omitempty"`
	UpstreamStatusCode int             `json:"upstream_status_code,omitempty"`
	UpstreamRequestID  string          `json:"upstream_request_id,omitempty"`
	RetryAfterMS       int             `json:"retry_after_ms,omitempty"`
	ErrorType          string          `json:"error_type,omitempty"`
	ErrorMessage       string          `json:"error_message,omitempty"`
	ExecutionState     string          `json:"execution_state,omitempty"`
	RequestBody        json.RawMessage `json:"request_body,omitempty"`
	ResponseBody       json.RawMessage `json:"response_body,omitempty"`
	// CommitState separates gateway-owned compatibility frames from model
	// output. WroteClientResponse remains on the wire for rolling upgrades.
	CommitState         string `json:"commit_state,omitempty"`
	WroteClientResponse bool   `json:"wrote_client_response"`
	SupportsStagedRetry bool   `json:"supports_staged_retry,omitempty"`
}

func (r OpenAIEdgeRetryRequest) EffectiveCommitState() string {
	switch r.CommitState {
	case OpenAIEdgeCommitStateNone:
		if r.WroteClientResponse {
			return OpenAIEdgeCommitStateRealOutput
		}
		return OpenAIEdgeCommitStateNone
	case OpenAIEdgeCommitStateGatewayOnly,
		OpenAIEdgeCommitStateRealOutput,
		OpenAIEdgeCommitStateTerminal:
		return r.CommitState
	default:
		if r.WroteClientResponse {
			return OpenAIEdgeCommitStateRealOutput
		}
		return OpenAIEdgeCommitStateNone
	}
}

// OpenAIEdgeCommitRequest releases retry-only payload copies after the edge
// has a downstream response. It intentionally does not settle usage or slots.
type OpenAIEdgeCommitRequest struct {
	EdgeRequestID string `json:"edge_request_id"`
	LeaseID       string `json:"lease_id,omitempty"`
	AccountID     int64  `json:"account_id,omitempty"`
}

// OpenAIEdgeRenewRequest extends an active data-plane lease. The edge sends
// these heartbeats while an HTTP/SSE or WebSocket response is still alive so
// the short crash-recovery TTL does not cap legitimate long-running streams.
type OpenAIEdgeRenewRequest struct {
	EdgeRequestID string `json:"edge_request_id"`
	LeaseID       string `json:"lease_id"`
	AccountID     int64  `json:"account_id,omitempty"`
}

type OpenAIEdgeRetryDecision struct {
	Action            string          `json:"action"`
	Reason            string          `json:"reason,omitempty"`
	ContinuationToken string          `json:"continuation_token,omitempty"`
	StagedRetryID     string          `json:"staged_retry_id,omitempty"`
	Plan              *OpenAIEdgePlan `json:"plan,omitempty"`
	FailureRecorded   bool            `json:"failure_recorded,omitempty"`
	StatusCode        int             `json:"status_code,omitempty"`
	ErrorType         string          `json:"error_type,omitempty"`
	ErrorMessage      string          `json:"error_message,omitempty"`
	RetryAfterMS      int             `json:"retry_after_ms,omitempty"`
}

type OpenAIEdgeRetryStageRequest struct {
	EdgeRequestID string `json:"edge_request_id"`
	LeaseID       string `json:"lease_id"`
	AccountID     int64  `json:"account_id,omitempty"`
	StagedRetryID string `json:"staged_retry_id"`
	Action        string `json:"action"`
}

type OpenAIEdgeCompleteRequest struct {
	EdgeRequestID       string      `json:"edge_request_id"`
	LeaseID             string      `json:"lease_id,omitempty"`
	AccountID           int64       `json:"account_id,omitempty"`
	Success             bool        `json:"success"`
	FailureClass        string      `json:"failure_class,omitempty"`
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
	MaxSemanticGapMS    *int64      `json:"max_semantic_gap_ms,omitempty"`
	GuardSampleAtUnixNS *int64      `json:"guard_sample_at_unix_ns,omitempty"`
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
	FailureClass       string `json:"failure_class,omitempty"`
	ClientDisconnected bool   `json:"client_disconnected,omitempty"`
	RelayAttempted     bool   `json:"relay_attempted,omitempty"`
	FallbackToGo       bool   `json:"fallback_to_go,omitempty"`
}

type OpenAIEdgeRecoverRequest struct {
	EdgeNodeID     string `json:"edge_node_id"`
	EdgeInstanceID string `json:"edge_instance_id"`
}

type OpenAIEdgeAck struct {
	OK       bool   `json:"ok"`
	Reason   string `json:"reason,omitempty"`
	Released int    `json:"released,omitempty"`
}

func EncodeOpenAIEdgeRawBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(body)
}
