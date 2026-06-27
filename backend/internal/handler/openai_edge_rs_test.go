package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func newOpenAIEdgeTestContext(method, path, body, secret string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if secret != "" {
		c.Request.Header.Set(openAIEdgeSecretHeader, secret)
	}
	return c, w
}

func TestOpenAIEdgePrepareRequiresEnabledInternalAPIAndSecret(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	c, w := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/prepare", `{}`, "")

	h.OpenAIEdgePrepare(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected disabled internal API to return 404, got %d: %s", w.Code, w.Body.String())
	}

	h.cfg.Gateway.OpenAIEdgeRS.InternalAPIEnabled = true
	h.cfg.Gateway.OpenAIEdgeRS.InternalSecret = "edge-secret"
	c, w = newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/prepare", `{}`, "wrong-secret")

	h.OpenAIEdgePrepare(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid secret to return 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOpenAIEdgePrepareFallsBackUntilControlPlaneExtractionExists(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.StreamLowLatencyMode = config.StreamLowLatencyModeAggressive
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{
		Enabled:            true,
		InternalAPIEnabled: true,
		InternalSecret:     "edge-secret",
		Mode:               "relay",
		LeaseTTLMS:         12345,
	}
	c, w := newOpenAIEdgeTestContext(
		http.MethodPost,
		"/internal/edge/openai/prepare",
		`{"edge_request_id":"edge-1","method":"POST","path":"/v1/chat/completions","body":{"model":"gpt-4o","stream":true},"stream":true}`,
		"edge-secret",
	)

	h.OpenAIEdgePrepare(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected prepare 200, got %d: %s", w.Code, w.Body.String())
	}
	var plan service.OpenAIEdgePlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if plan.Action != service.OpenAIEdgeActionFallbackGo {
		t.Fatalf("expected fallback action, got %q", plan.Action)
	}
	if plan.Reason != "edge_dependencies_missing" {
		t.Fatalf("expected dependency fallback reason, got %q", plan.Reason)
	}
	if plan.EdgeRequestID != "edge-1" {
		t.Fatalf("expected edge request id to round-trip, got %q", plan.EdgeRequestID)
	}
	if plan.LeaseTTLMS != 12345 {
		t.Fatalf("expected lease ttl to round-trip, got %d", plan.LeaseTTLMS)
	}
	if plan.LowLatencyMode != config.StreamLowLatencyModeAggressive {
		t.Fatalf("expected low latency mode, got %q", plan.LowLatencyMode)
	}
}

func TestOpenAIEdgePrepareResponsesFallsBackUntilEligible(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{
		Enabled:            true,
		InternalAPIEnabled: true,
		InternalSecret:     "edge-secret",
		Mode:               "relay",
		LeaseTTLMS:         12345,
		RelayResponses:     true,
		RolloutPercent:     100,
	}
	c, w := newOpenAIEdgeTestContext(
		http.MethodPost,
		"/internal/edge/openai/prepare",
		`{"edge_request_id":"edge-resp-1","method":"POST","path":"/v1/responses","body":{"model":"gpt-4.1","stream":true,"input":"hello"},"stream":true}`,
		"edge-secret",
	)

	h.OpenAIEdgePrepare(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected prepare 200, got %d: %s", w.Code, w.Body.String())
	}
	var plan service.OpenAIEdgePlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if plan.Action != service.OpenAIEdgeActionFallbackGo {
		t.Fatalf("expected fallback action, got %q", plan.Action)
	}
	if plan.Reason != "edge_dependencies_missing" {
		t.Fatalf("expected dependency fallback reason, got %q", plan.Reason)
	}
	if plan.EdgeRequestID != "edge-resp-1" {
		t.Fatalf("expected edge request id to round-trip, got %q", plan.EdgeRequestID)
	}
}

func TestOpenAIEdgePrepareResponsesWSDisabledFallsBack(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{
		Enabled:                 true,
		InternalAPIEnabled:      true,
		InternalSecret:          "edge-secret",
		Mode:                    "relay",
		LeaseTTLMS:              12345,
		RelayResponses:          true,
		RelayResponsesWebSocket: false,
		RolloutPercent:          100,
	}
	c, w := newOpenAIEdgeTestContext(
		http.MethodPost,
		"/internal/edge/openai/prepare",
		`{"edge_request_id":"edge-ws-1","method":"GET","path":"/v1/responses","headers":{"Upgrade":"websocket"},"body":{"model":"gpt-4.1","input":"hello"},"stream":true}`,
		"edge-secret",
	)

	h.OpenAIEdgePrepare(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected prepare 200, got %d: %s", w.Code, w.Body.String())
	}
	var plan service.OpenAIEdgePlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if plan.Action != service.OpenAIEdgeActionFallbackGo {
		t.Fatalf("expected fallback action, got %q", plan.Action)
	}
	if plan.Reason != "edge_responses_ws_relay_disabled" {
		t.Fatalf("expected ws disabled fallback reason, got %q", plan.Reason)
	}
}

func TestOpenAIEdgeToolOutputFallbackReason(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no_tool_output",
			body: `{"input":"hello"}`,
			want: "",
		},
		{
			name: "self_contained_tool_context",
			body: `{"input":[{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`,
			want: "",
		},
		{
			name: "item_reference_covers_call_id",
			body: `{"input":[{"type":"item_reference","id":"call_1"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`,
			want: "",
		},
		{
			name: "missing_call_id",
			body: `{"input":[{"type":"function_call_output","output":"ok"}]}`,
			want: "function_call_output_missing_call_id_requires_go",
		},
		{
			name: "orphan_tool_output",
			body: `{"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`,
			want: "function_call_output_requires_go",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := openAIEdgeToolOutputFallbackReason([]byte(tc.body)); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestOpenAIEdgeRolloutRejectReason(t *testing.T) {
	groupID := int64(20)
	apiKey := &service.APIKey{ID: 10, GroupID: &groupID}

	if reason := openAIEdgeRolloutRejectReason(config.GatewayOpenAIEdgeRSConfig{
		AllowedAPIKeyIDs: []int64{11},
		RolloutPercent:   100,
	}, apiKey, "gpt-4.1", "edge-1"); reason != "edge_api_key_not_in_rollout" {
		t.Fatalf("expected api key rollout rejection, got %q", reason)
	}
	if reason := openAIEdgeRolloutRejectReason(config.GatewayOpenAIEdgeRSConfig{
		AllowedGroupIDs: []int64{21},
		RolloutPercent:  100,
	}, apiKey, "gpt-4.1", "edge-1"); reason != "edge_group_not_in_rollout" {
		t.Fatalf("expected group rollout rejection, got %q", reason)
	}
	if reason := openAIEdgeRolloutRejectReason(config.GatewayOpenAIEdgeRSConfig{
		AllowedModels:  []string{"gpt-4o"},
		RolloutPercent: 100,
	}, apiKey, "gpt-4.1", "edge-1"); reason != "edge_model_not_in_rollout" {
		t.Fatalf("expected model rollout rejection, got %q", reason)
	}
	if reason := openAIEdgeRolloutRejectReason(config.GatewayOpenAIEdgeRSConfig{
		RolloutPercent: 0,
	}, apiKey, "gpt-4.1", "edge-1"); reason != "edge_rollout_percent_zero" {
		t.Fatalf("expected zero rollout rejection, got %q", reason)
	}
	if reason := openAIEdgeRolloutRejectReason(config.GatewayOpenAIEdgeRSConfig{
		AllowedAPIKeyIDs: []int64{10},
		AllowedGroupIDs:  []int64{20},
		AllowedModels:    []string{"GPT-4.1"},
		RolloutPercent:   100,
	}, apiKey, "gpt-4.1", "edge-1"); reason != "" {
		t.Fatalf("expected rollout to pass, got %q", reason)
	}
}

func TestOpenAIEdgeCallbacksRequireSecretAndAck(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{
		InternalAPIEnabled: true,
		InternalSecret:     "edge-secret",
	}

	c, w := newOpenAIEdgeTestContext(
		http.MethodPost,
		"/internal/edge/openai/retry",
		`{"edge_request_id":"edge-1","wrote_client_response":false}`,
		"edge-secret",
	)
	h.OpenAIEdgeRetry(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected retry 200, got %d: %s", w.Code, w.Body.String())
	}
	var retry service.OpenAIEdgeRetryDecision
	if err := json.Unmarshal(w.Body.Bytes(), &retry); err != nil {
		t.Fatalf("unmarshal retry decision: %v", err)
	}
	if retry.Action != service.OpenAIEdgeActionFallbackGo {
		t.Fatalf("expected retry fallback, got %q", retry.Action)
	}

	for _, tc := range []struct {
		name string
		call func(*gin.Context)
		body string
	}{
		{name: "complete", call: h.OpenAIEdgeComplete, body: `{"edge_request_id":"edge-1","success":true}`},
		{name: "abort", call: h.OpenAIEdgeAbort, body: `{"edge_request_id":"edge-1","reason":"client_disconnect"}`},
	} {
		c, w := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/"+tc.name, tc.body, "edge-secret")
		tc.call(c)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", tc.name, w.Code, w.Body.String())
		}
		var ack service.OpenAIEdgeAck
		if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
			t.Fatalf("%s: unmarshal ack: %v", tc.name, err)
		}
		if !ack.OK {
			t.Fatalf("%s: expected ack ok", tc.name)
		}
	}
}

func TestOpenAIEdgeRetryFallbackBoundaries(t *testing.T) {
	h := &OpenAIGatewayHandler{
		openAIEdgeLeases: map[string]*openAIEdgeLease{
			"lease-1": {
				leaseID: "lease-1",
				account: &service.Account{ID: 1001},
			},
		},
	}
	c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/retry", `{}`, "")

	for _, tc := range []struct {
		name string
		req  service.OpenAIEdgeRetryRequest
		want string
	}{
		{
			name: "written client response cannot retry",
			req: service.OpenAIEdgeRetryRequest{
				LeaseID:             "lease-1",
				UpstreamStatusCode:  http.StatusTooManyRequests,
				WroteClientResponse: true,
			},
			want: "client_response_already_written",
		},
		{
			name: "missing lease falls back",
			req: service.OpenAIEdgeRetryRequest{
				LeaseID:            "missing",
				UpstreamStatusCode: http.StatusTooManyRequests,
			},
			want: "lease_not_found",
		},
		{
			name: "account mismatch falls back",
			req: service.OpenAIEdgeRetryRequest{
				LeaseID:            "lease-1",
				AccountID:          2002,
				UpstreamStatusCode: http.StatusTooManyRequests,
			},
			want: "account_mismatch",
		},
		{
			name: "non retryable upstream status falls back",
			req: service.OpenAIEdgeRetryRequest{
				LeaseID:            "lease-1",
				UpstreamStatusCode: http.StatusBadRequest,
			},
			want: "upstream_status_not_retryable",
		},
		{
			name: "request timeout passes gate then fails on deps",
			req: service.OpenAIEdgeRetryRequest{
				LeaseID:            "lease-1",
				UpstreamStatusCode: http.StatusRequestTimeout,
			},
			want: "edge_dependencies_missing",
		},
		{
			name: "server error passes gate then fails on deps",
			req: service.OpenAIEdgeRetryRequest{
				LeaseID:            "lease-1",
				UpstreamStatusCode: http.StatusInternalServerError,
			},
			want: "edge_dependencies_missing",
		},
		{
			name: "missing dependencies falls back after retryable status",
			req: service.OpenAIEdgeRetryRequest{
				LeaseID:            "lease-1",
				UpstreamStatusCode: http.StatusTooManyRequests,
			},
			want: "edge_dependencies_missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := h.openAIEdgeRetryDecision(c, tc.req)
			if decision.Action != service.OpenAIEdgeActionFallbackGo {
				t.Fatalf("expected fallback action, got %q", decision.Action)
			}
			if decision.Reason != tc.want {
				t.Fatalf("expected reason %q, got %q", tc.want, decision.Reason)
			}
		})
	}
}

func TestOpenAIEdgeRetryProtectsPoolModelRouting404(t *testing.T) {
	h := &OpenAIGatewayHandler{
		openAIEdgeLeases: map[string]*openAIEdgeLease{
			"lease-1": {
				leaseID: "lease-1",
				account: &service.Account{
					ID:       1001,
					Platform: service.PlatformOpenAI,
					Type:     service.AccountTypeAPIKey,
					Credentials: map[string]any{
						"pool_mode": true,
					},
				},
			},
		},
	}
	body := `{"error":{"message":"Model \"gpt-5.4-mini\" is not supported by any configured account in this group","type":"model_not_found"}}`
	c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/retry", `{}`, "")

	decision := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{
		LeaseID:            "lease-1",
		AccountID:          1001,
		UpstreamStatusCode: http.StatusNotFound,
		ErrorMessage:       body,
		ResponseBody:       json.RawMessage(strconv.Quote(body)),
	})

	if decision.Action != service.OpenAIEdgeActionRespondError {
		t.Fatalf("expected protected response action, got %q (%s)", decision.Action, decision.Reason)
	}
	if decision.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected sanitized 400, got %d", decision.StatusCode)
	}
	if decision.ErrorType != "invalid_request_error" {
		t.Fatalf("expected sanitized error type, got %q", decision.ErrorType)
	}
	if decision.ErrorMessage != service.OpenAIPoolModelRoutingClientMessage() {
		t.Fatalf("expected sanitized error message, got %q", decision.ErrorMessage)
	}
}

func TestOpenAIEdgeCompleteRejectsAccountMismatch(t *testing.T) {
	h := &OpenAIGatewayHandler{
		cfg: &config.Config{},
		openAIEdgeLeases: map[string]*openAIEdgeLease{
			"lease-1": {
				leaseID: "lease-1",
				account: &service.Account{ID: 1001},
			},
		},
	}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{
		InternalAPIEnabled: true,
		InternalSecret:     "edge-secret",
	}

	c, w := newOpenAIEdgeTestContext(
		http.MethodPost,
		"/internal/edge/openai/complete",
		`{"edge_request_id":"edge-1","lease_id":"lease-1","account_id":2002,"success":true}`,
		"edge-secret",
	)
	h.OpenAIEdgeComplete(c)

	var ack service.OpenAIEdgeAck
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if !ack.OK || ack.Reason != "account_mismatch" {
		t.Fatalf("expected account mismatch ack, got %#v", ack)
	}
}

func TestOpenAIEdgeRetryResponseBodyUnwrapsJSONString(t *testing.T) {
	body := openAIEdgeRetryResponseBody(service.OpenAIEdgeRetryRequest{
		ResponseBody: json.RawMessage(`"upstream said no"`),
	})
	if string(body) != "upstream said no" {
		t.Fatalf("expected JSON string body to unwrap, got %q", string(body))
	}

	raw := openAIEdgeRetryResponseBody(service.OpenAIEdgeRetryRequest{
		ResponseBody: json.RawMessage(`{"error":{"message":"nope"}}`),
	})
	if string(raw) != `{"error":{"message":"nope"}}` {
		t.Fatalf("expected raw object body to be preserved, got %q", string(raw))
	}
}

func TestOpenAIEdgeLeaseRoutingModelFallsBackToRequestModel(t *testing.T) {
	lease := &openAIEdgeLease{requestModel: "mapped-model"}
	if got := lease.openAIRoutingModel(); got != "mapped-model" {
		t.Fatalf("expected request model fallback, got %q", got)
	}
	lease.routingModel = "client-model"
	if got := lease.openAIRoutingModel(); got != "client-model" {
		t.Fatalf("expected routing model, got %q", got)
	}
}

func TestNormalizeOpenAIEdgePrepareBodyPrefersRawBase64(t *testing.T) {
	raw := []byte(`{"model":"gpt-4.1","stream":true,"input":"hello"}`)
	req := &service.OpenAIEdgePrepareRequest{
		Body:          json.RawMessage(`{"model":"wrong"}`),
		BodyRawBase64: base64.StdEncoding.EncodeToString(raw),
	}

	if err := normalizeOpenAIEdgePrepareBody(req); err != nil {
		t.Fatalf("normalize raw body: %v", err)
	}
	if string(req.Body) != string(raw) {
		t.Fatalf("expected raw body to win, got %s", string(req.Body))
	}
}
