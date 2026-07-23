package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
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

func newOpenAIEdgeRetryWSTestService() *service.OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = service.OpenAIWSIngressModeCtxPool
	return service.NewOpenAIGatewayService(
		nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
}

func newOpenAIEdgeRetryWSTestAccount(id int64, strongIsolation bool) *service.Account {
	credentials := map[string]any{"access_token": "oauth-token"}
	if strongIsolation {
		credentials["upstream_strong_isolation_enabled"] = true
	}
	return &service.Account{
		ID:          id,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Concurrency: 1,
		Credentials: credentials,
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_mode": service.OpenAIWSIngressModePassthrough,
		},
	}
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

func TestTakeOpenAIEdgeLeaseRejectsStaleCompletionWithoutRemovingCurrentLease(t *testing.T) {
	h := &OpenAIGatewayHandler{openAIEdgeLeases: make(map[string]*openAIEdgeLease)}
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-1",
		leaseID:       "lease-1",
		account:       &service.Account{ID: 22},
	}
	h.openAIEdgeLeases[lease.leaseID] = lease

	got, reason := h.takeOpenAIEdgeLeaseForRequest("lease-1", "edge-1", 11, true)
	if got != nil || reason != "account_mismatch" {
		t.Fatalf("stale completion result = (%v, %q), want account_mismatch", got, reason)
	}
	if h.openAIEdgeLeases[lease.leaseID] != lease || lease.settled {
		t.Fatal("stale completion removed or settled the current lease")
	}

	got, reason = h.takeOpenAIEdgeLeaseForRequest("lease-1", "edge-1", 22, true)
	if got != lease || reason != "" {
		t.Fatalf("current completion result = (%v, %q), want lease", got, reason)
	}
	if h.openAIEdgeLeases[lease.leaseID] != nil || !lease.settled {
		t.Fatal("current completion did not atomically settle the lease")
	}
}

func TestTakeOpenAIEdgeLeaseAbortTracksRequestAcrossAccountSwitch(t *testing.T) {
	h := &OpenAIGatewayHandler{openAIEdgeLeases: make(map[string]*openAIEdgeLease)}
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-2",
		leaseID:       "lease-2",
		account:       &service.Account{ID: 202},
	}
	h.openAIEdgeLeases[lease.leaseID] = lease

	got, reason := h.takeOpenAIEdgeLeaseForRequest("lease-2", "edge-2", 101, false)
	if got != lease || reason != "" {
		t.Fatalf("abort after account switch result = (%v, %q), want lease", got, reason)
	}
}

func TestCancelOpenAIEdgeLeaseByRequestIDReleasesStoredLease(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	accountReleaseCalls := 0
	userReleaseCalls := 0
	lease := &openAIEdgeLease{
		edgeRequestID:      "edge-cancel-1",
		leaseID:            "lease-cancel-1",
		account:            &service.Account{ID: 202},
		accountReleaseFunc: func() { accountReleaseCalls++ },
		userReleaseFunc:    func() { userReleaseCalls++ },
	}
	if !h.storeOpenAIEdgeLease(lease, time.Minute) {
		t.Fatal("expected lease to be stored")
	}

	got, reason := h.cancelOpenAIEdgeLeaseForRequest("", "edge-cancel-1", 0, time.Minute)
	if reason != "" || got != lease {
		t.Fatalf("cancel result = (%v, %q), want stored lease", got, reason)
	}
	got.release()

	if accountReleaseCalls != 1 || userReleaseCalls != 1 {
		t.Fatalf("expected both slots to be released once, got account=%d user=%d", accountReleaseCalls, userReleaseCalls)
	}
	if h.openAIEdgeLeases[lease.leaseID] != nil || h.openAIEdgeLeaseByRequest[lease.edgeRequestID] != "" {
		t.Fatal("cancel did not remove both lease indexes")
	}
}

func TestCancelOpenAIEdgeLeaseRejectsAccountMismatch(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-cancel-account",
		leaseID:       "lease-cancel-account",
		account:       &service.Account{ID: 202},
	}
	if !h.storeOpenAIEdgeLease(lease, time.Minute) {
		t.Fatal("expected lease to be stored")
	}

	got, reason := h.cancelOpenAIEdgeLeaseForRequest(lease.leaseID, lease.edgeRequestID, 101, time.Minute)
	if got != nil || reason != "account_mismatch" {
		t.Fatalf("cancel result = (%v, %q), want account_mismatch", got, reason)
	}
	if h.openAIEdgeLeases[lease.leaseID] != lease || lease.settled {
		t.Fatal("account mismatch removed or settled the active lease")
	}

	got, reason = h.cancelOpenAIEdgeLeaseForRequest(lease.leaseID, lease.edgeRequestID, 202, time.Minute)
	if got != lease || reason != "" {
		t.Fatalf("matching cancel result = (%v, %q), want stored lease", got, reason)
	}
}

func TestStoreOpenAIEdgeLeaseRejectsLatePrepareAfterCancellation(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	if lease, reason := h.cancelOpenAIEdgeLeaseForRequest("", "edge-late-1", 0, time.Minute); lease != nil || reason != "" {
		t.Fatalf("pre-cancel result = (%v, %q), want empty success", lease, reason)
	}
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-late-1",
		leaseID:       "lease-late-1",
	}
	if h.storeOpenAIEdgeLease(lease, time.Minute) {
		t.Fatal("late prepare lease must be rejected after request cancellation")
	}
	if h.openAIEdgeLeases[lease.leaseID] != nil {
		t.Fatal("rejected late prepare lease was retained")
	}
}

func TestOpenAIEdgeLeaseExpiryMarksSettledAndReleases(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	released := make(chan struct{}, 1)
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-expiry-1",
		leaseID:       "lease-expiry-1",
		accountReleaseFunc: func() {
			released <- struct{}{}
		},
	}
	if !h.storeOpenAIEdgeLease(lease, time.Millisecond) {
		t.Fatal("expected lease to be stored")
	}

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lease expiry release")
	}

	lease.mu.Lock()
	settled := lease.settled
	lease.mu.Unlock()
	if !settled {
		t.Fatal("expired lease must be marked settled before its slot is released")
	}
	if h.getOpenAIEdgeLease(lease.leaseID) != nil || h.openAIEdgeLeaseByRequest[lease.edgeRequestID] != "" {
		t.Fatal("expired lease was retained in an index")
	}
}

func TestRecoverOpenAIEdgeLeasesOnlyReleasesPreviousInstanceOnSameNode(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	oldReleaseCalls := 0
	currentReleaseCalls := 0
	otherNodeReleaseCalls := 0
	leases := []*openAIEdgeLease{
		{
			edgeRequestID:      "edge-old",
			edgeNodeID:         "node-1",
			edgeInstanceID:     "instance-old",
			leaseID:            "lease-old",
			accountReleaseFunc: func() { oldReleaseCalls++ },
		},
		{
			edgeRequestID:      "edge-current",
			edgeNodeID:         "node-1",
			edgeInstanceID:     "instance-current",
			leaseID:            "lease-current",
			accountReleaseFunc: func() { currentReleaseCalls++ },
		},
		{
			edgeRequestID:      "edge-other",
			edgeNodeID:         "node-2",
			edgeInstanceID:     "instance-old",
			leaseID:            "lease-other",
			accountReleaseFunc: func() { otherNodeReleaseCalls++ },
		},
	}
	for _, lease := range leases {
		if !h.storeOpenAIEdgeLease(lease, time.Minute) {
			t.Fatalf("store %s failed", lease.leaseID)
		}
	}

	if released := h.recoverOpenAIEdgeLeases("node-1", "instance-current"); released != 1 {
		t.Fatalf("released=%d, want 1", released)
	}
	if oldReleaseCalls != 1 || currentReleaseCalls != 0 || otherNodeReleaseCalls != 0 {
		t.Fatalf("unexpected release counts old=%d current=%d other=%d", oldReleaseCalls, currentReleaseCalls, otherNodeReleaseCalls)
	}
	if h.openAIEdgeLeases["lease-old"] != nil || h.openAIEdgeLeases["lease-current"] == nil || h.openAIEdgeLeases["lease-other"] == nil {
		t.Fatal("recovery removed the wrong lease set")
	}
	if expiresAt := h.openAIEdgeCancelled["edge-old"]; !expiresAt.After(time.Now()) {
		t.Fatal("recovery must tombstone old request IDs against delayed prepare delivery")
	}

	for _, leaseID := range []string{"lease-current", "lease-other"} {
		lease, _ := h.takeOpenAIEdgeLeaseForRequest(leaseID, "", 0, false)
		if lease != nil {
			lease.release()
		}
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

func TestOpenAIEdgePrepareResponsesWSFallsBackForPerTurnGovernance(t *testing.T) {
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
	if plan.Reason != "edge_ws_requires_go_per_turn_governance" {
		t.Fatalf("expected per-turn governance fallback reason, got %q", plan.Reason)
	}
}

func TestOpenAIEdgeResponsesWSAccountFallbackReason(t *testing.T) {
	strongIsolation := &service.Account{
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"upstream_strong_isolation_enabled": true,
		},
	}
	if got := openAIEdgeResponsesWSAccountFallbackReason(strongIsolation); got != "edge_ws_strong_isolation_requires_go" {
		t.Fatalf("expected strong-isolation fallback, got %q", got)
	}

	ordinary := &service.Account{
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"upstream_strong_isolation_enabled": false,
		},
	}
	if got := openAIEdgeResponsesWSAccountFallbackReason(ordinary); got != "" {
		t.Fatalf("expected ordinary account to remain eligible, got %q", got)
	}
	if got := openAIEdgeResponsesWSAccountFallbackReason(nil); got != "" {
		t.Fatalf("expected nil account to remain a no-op, got %q", got)
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
		{name: "recover", call: h.OpenAIEdgeRecover, body: `{"edge_node_id":"node-1","edge_instance_id":"instance-1"}`},
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
				edgeRequestID: "edge-1",
				leaseID:       "lease-1",
				account:       &service.Account{ID: 1001},
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

func TestOpenAIEdgeRetryCacheCreationCompatibilityStaysOnEdgeRS(t *testing.T) {
	cfg := &config.Config{}
	gatewaySvc := service.NewOpenAIGatewayService(
		nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	stable := strings.Repeat("stable system policy ", 260)
	forwardBody := []byte(`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"system","content":"` + stable + `"},{"role":"user","content":"hello"}]}`)
	account := &service.Account{
		ID:          1001,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
			"openai_prompt_cache_creation_optimization_enabled": true,
			"openai_prompt_cache_creation_optimization_mode":    service.OpenAIPromptCacheCreationOptimizationModeSuppress,
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}
	h := &OpenAIGatewayHandler{
		gatewayService: gatewaySvc,
		openAIEdgeLeases: map[string]*openAIEdgeLease{
			"lease-1": {
				edgeRequestID:      "edge-1",
				leaseID:            "lease-1",
				expiresAt:          time.Now().Add(time.Minute),
				account:            account,
				cachePolicyApplied: true,
				forwardBody:        forwardBody,
				inboundEndpoint:    "/v1/chat/completions",
			},
		},
	}
	c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/retry", `{}`, "")
	errorBody := `{"error":{"message":"Unsupported parameter: prompt_cache_options"}}`

	decision := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{
		LeaseID:            "lease-1",
		AccountID:          account.ID,
		UpstreamStatusCode: http.StatusBadRequest,
		ErrorMessage:       errorBody,
		ResponseBody:       json.RawMessage(strconv.Quote(errorBody)),
	})

	if decision.Action != service.OpenAIEdgeActionRelay || decision.Plan == nil {
		t.Fatalf("expected edge-rs relay retry, got action=%q reason=%q", decision.Action, decision.Reason)
	}
	if decision.Reason != "cache_creation_optimization_unsupported" {
		t.Fatalf("unexpected retry reason: %q", decision.Reason)
	}
	decoded, err := base64.StdEncoding.DecodeString(decision.Plan.BodyRawBase64)
	if err != nil {
		t.Fatalf("decode retry body: %v", err)
	}
	if gjson.GetBytes(decoded, "prompt_cache_options").Exists() {
		t.Fatalf("fallback edge plan must use the account default request policy: %s", decoded)
	}
	if decision.Plan.PromptCacheCreationOptimizationMode != "" {
		t.Fatalf("fallback edge plan must clear optimization mode, got %q", decision.Plan.PromptCacheCreationOptimizationMode)
	}
	lease := h.openAIEdgeLeases["lease-1"]
	if lease == nil || lease.cachePolicyEnabled || lease.cachePolicyApplied {
		t.Fatal("fallback edge retry must clear the lease policy marker to prevent a retry loop")
	}
	if !account.IsOpenAIPromptCacheCreationSuppressEnabled() {
		t.Fatal("fallback edge retry must not mutate the scheduler account credentials")
	}
}

func TestOpenAIEdgeRetryCommittedCacheCreationCompatibilityRecordsBackoff(t *testing.T) {
	gatewaySvc := service.NewOpenAIGatewayService(
		nil, nil, nil, nil, nil, nil, nil, &config.Config{}, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	account := &service.Account{
		ID:       1001,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"openai_prompt_cache_creation_optimization_enabled": true,
		},
	}
	h := &OpenAIGatewayHandler{
		gatewayService: gatewaySvc,
		openAIEdgeLeases: map[string]*openAIEdgeLease{
			"lease-1": {
				leaseID:            "lease-1",
				account:            account,
				cachePolicyApplied: true,
			},
		},
	}
	c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/retry", `{}`, "")
	errorBody := `{"error":{"message":"Unsupported parameter: prompt_cache_options"}}`
	decision := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{
		LeaseID:             "lease-1",
		AccountID:           account.ID,
		UpstreamStatusCode:  http.StatusBadRequest,
		ErrorMessage:        errorBody,
		ResponseBody:        json.RawMessage(strconv.Quote(errorBody)),
		WroteClientResponse: true,
	})

	if decision.Reason != "client_response_already_written" {
		t.Fatalf("unexpected decision: action=%q reason=%q", decision.Action, decision.Reason)
	}
	if gatewaySvc.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account) {
		t.Fatal("committed compatibility failure must disable the policy for subsequent requests")
	}
}

func TestOpenAIEdgeRetryExplicitRejectedResponsesFieldsStayOnSameLease(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","stream":true,"max_output_tokens":2048,"input":[{"type":"custom_tool_call","namespace":"tools","input":"{}"}]}`)
	account := &service.Account{ID: 1001, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	lease := &openAIEdgeLease{
		edgeRequestID:      "edge-1",
		leaseID:            "lease-1",
		expiresAt:          time.Now().Add(time.Minute),
		account:            account,
		inboundEndpoint:    "/v1/responses",
		forwardBody:        append([]byte(nil), body...),
		lastPlan:           service.OpenAIEdgePlan{EdgeRequestID: "edge-1", LeaseID: "lease-1", AccountID: account.ID, Headers: map[string]string{"Authorization": "Bearer stable"}, BodyRawBase64: service.EncodeOpenAIEdgeRawBody(body)},
		sameAccountRetries: map[int64]int{},
		failedAccountIDs:   map[int64]struct{}{},
	}
	h := &OpenAIGatewayHandler{openAIEdgeLeases: map[string]*openAIEdgeLease{"lease-1": lease}}
	c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/retry", `{}`, "")

	first := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{
		LeaseID:            lease.leaseID,
		AccountID:          account.ID,
		UpstreamStatusCode: http.StatusBadRequest,
		ResponseBody:       json.RawMessage(`{"error":{"code":"unknown_parameter","param":"input[0].namespace","message":"Unknown parameter: input[0].namespace"}}`),
	})
	if first.Action != service.OpenAIEdgeActionRelay || first.Plan == nil {
		t.Fatalf("expected same-lease field retry, got action=%q reason=%q", first.Action, first.Reason)
	}
	firstBody, err := base64.StdEncoding.DecodeString(first.Plan.BodyRawBase64)
	if err != nil {
		t.Fatalf("decode first retry body: %v", err)
	}
	if gjson.GetBytes(firstBody, "input.0.namespace").Exists() || !gjson.GetBytes(firstBody, "max_output_tokens").Exists() {
		t.Fatalf("first retry removed the wrong field: %s", firstBody)
	}
	if !bytes.Equal(lease.forwardBody, firstBody) {
		t.Fatalf("first retry did not update failover body: plan=%s forward=%s", firstBody, lease.forwardBody)
	}

	second := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{
		LeaseID:            lease.leaseID,
		AccountID:          account.ID,
		UpstreamStatusCode: http.StatusBadRequest,
		ResponseBody:       json.RawMessage(`{"error":{"code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: max_output_tokens"}}`),
	})
	if second.Action != service.OpenAIEdgeActionRelay || second.Plan == nil {
		t.Fatalf("expected second same-lease field retry, got action=%q reason=%q", second.Action, second.Reason)
	}
	secondBody, err := base64.StdEncoding.DecodeString(second.Plan.BodyRawBase64)
	if err != nil {
		t.Fatalf("decode second retry body: %v", err)
	}
	if gjson.GetBytes(secondBody, "input.0.namespace").Exists() || gjson.GetBytes(secondBody, "max_output_tokens").Exists() {
		t.Fatalf("second retry did not preserve both removals: %s", secondBody)
	}
	if !bytes.Equal(lease.forwardBody, secondBody) {
		t.Fatalf("second retry did not update failover body: plan=%s forward=%s", secondBody, lease.forwardBody)
	}
	if len(lease.rejectedFields) != 2 {
		t.Fatalf("expected two confirmed rejected fields, got %v", lease.rejectedFields)
	}
	if second.Plan.AccountID != account.ID || second.Plan.LeaseID != lease.leaseID || second.Plan.Headers["Authorization"] != "Bearer stable" {
		t.Fatal("compatibility retry changed account, lease, or authentication headers")
	}
	if lease.switchCount != 0 || len(lease.sameAccountRetries) != 0 || len(lease.failedAccountIDs) != 0 {
		t.Fatal("compatibility retry polluted ordinary retry or failover state")
	}
}

func TestOpenAIEdgeRetryRejectedFieldsSurviveCachePolicyFallback(t *testing.T) {
	cfg := &config.Config{}
	gatewaySvc := service.NewOpenAIGatewayService(
		nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	canonicalBody := []byte(`{"model":"gpt-5.6-sol","stream":true,"max_tokens":2048,"input":[{"type":"custom_tool_call","namespace":"tools","input":"{}"}]}`)
	account := &service.Account{
		ID: 1002, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
			"openai_prompt_cache_creation_optimization_enabled": true,
			"openai_prompt_cache_creation_optimization_mode":    service.OpenAIPromptCacheCreationOptimizationModeSuppress,
		},
		Extra: map[string]any{
			"openai_passthrough":                     true,
			"openai_responses_passthrough_compat":    true,
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}
	c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/retry", `{}`, "")
	prepared, err := gatewaySvc.BuildRawResponsesEdgePlan(c.Request.Context(), c, account, canonicalBody)
	if err != nil {
		t.Fatalf("build initial optimized plan: %v", err)
	}
	initialBody, err := openAIEdgePlanBody(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(initialBody, "prompt_cache_options").Exists() || !gjson.GetBytes(initialBody, "max_output_tokens").Exists() {
		t.Fatalf("test setup did not apply cache policy and token alias: %s", initialBody)
	}

	prepared.Plan.EdgeRequestID = "edge-2"
	prepared.Plan.LeaseID = "lease-2"
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-2", leaseID: "lease-2", expiresAt: time.Now().Add(time.Minute),
		account: account, cachePolicyEnabled: true, cachePolicyApplied: true,
		forwardBody: append([]byte(nil), canonicalBody...), lastPlan: prepared.Plan,
		inboundEndpoint: "/v1/responses", sameAccountRetries: map[int64]int{}, failedAccountIDs: map[int64]struct{}{},
	}
	h := &OpenAIGatewayHandler{gatewayService: gatewaySvc, openAIEdgeLeases: map[string]*openAIEdgeLease{lease.leaseID: lease}}

	for _, rejection := range []struct {
		field string
		body  string
	}{
		{"input[0].namespace", `{"error":{"code":"unknown_parameter","param":"input[0].namespace","message":"Unknown parameter: input[0].namespace"}}`},
		{"max_output_tokens", `{"error":{"code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: max_output_tokens"}}`},
	} {
		decision := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{
			LeaseID: lease.leaseID, AccountID: account.ID, UpstreamStatusCode: http.StatusBadRequest,
			ResponseBody: json.RawMessage(rejection.body),
		})
		if decision.Action != service.OpenAIEdgeActionRelay || decision.Plan == nil || !strings.Contains(decision.Reason, "responses_rejected_field_") {
			t.Fatalf("field %s retry failed: action=%q reason=%q", rejection.field, decision.Action, decision.Reason)
		}
	}

	cacheError := `{"error":{"code":"unknown_parameter","param":"prompt_cache_options","message":"Unsupported parameter: prompt_cache_options"}}`
	decision := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{
		LeaseID: lease.leaseID, AccountID: account.ID, UpstreamStatusCode: http.StatusBadRequest,
		ErrorMessage: cacheError, ResponseBody: json.RawMessage(cacheError),
	})
	if decision.Action != service.OpenAIEdgeActionRelay || decision.Plan == nil || decision.Reason != "cache_creation_optimization_unsupported" {
		t.Fatalf("cache fallback failed: action=%q reason=%q", decision.Action, decision.Reason)
	}
	fallbackBody, err := openAIEdgePlanBody(*decision.Plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"input.0.namespace", "max_tokens", "max_output_tokens", "prompt_cache_options"} {
		if gjson.GetBytes(fallbackBody, path).Exists() {
			t.Fatalf("fallback resurrected rejected or account-policy field %s: %s", path, fallbackBody)
		}
	}
	if len(lease.rejectedFields) != 2 || lease.cachePolicyEnabled || lease.cachePolicyApplied {
		t.Fatalf("unexpected fallback state: fields=%v enabled=%v applied=%v", lease.rejectedFields, lease.cachePolicyEnabled, lease.cachePolicyApplied)
	}
	if !account.IsOpenAIPromptCacheCreationSuppressEnabled() {
		t.Fatal("fallback mutated the scheduler account policy")
	}
}

func TestOpenAIEdgeRetryEndpointEligibilityPreservesTransportContracts(t *testing.T) {
	wsAccount := newOpenAIEdgeRetryWSTestAccount(1001, false)
	if got := openAIEdgeRetryRequiredTransport("/v1/responses:ws"); got != service.OpenAIUpstreamTransportResponsesWebsocketV2 {
		t.Fatalf("WS retry transport = %q, want ws_v2", got)
	}
	if !openAIEdgeRetryAccountEligible(wsAccount, "/v1/responses:ws") {
		t.Fatal("ordinary WSv2 account must remain eligible for an edge retry")
	}
	if openAIEdgeRetryAccountEligible(newOpenAIEdgeRetryWSTestAccount(1002, true), "/v1/responses:ws") {
		t.Fatal("strong-isolation WS account must remain on the Go HTTP bridge")
	}

	rawResponsesAccount := &service.Account{
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra: map[string]any{
			"openai_passthrough":                     true,
			"openai_responses_passthrough_compat":    true,
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}
	if got := openAIEdgeRetryRequiredTransport("/v1/responses"); got != service.OpenAIUpstreamTransportAny {
		t.Fatalf("HTTP retry transport = %q, want scheduler default", got)
	}
	if got, want := openAIEdgeRetryAccountEligible(rawResponsesAccount, "/v1/responses"), service.IsOpenAIEdgeRawRelayEligibleForInboundEndpoint(rawResponsesAccount, "/v1/responses"); got != want || !got {
		t.Fatalf("HTTP Responses eligibility changed: got=%v want=%v", got, want)
	}
	if openAIEdgeRetryAccountEligible(wsAccount, "/v1/responses") {
		t.Fatal("WS-only OAuth account must not become eligible for raw HTTP Responses retry")
	}
}

func TestOpenAIEdgeRetryResponsesWSRebuildRetainsTransportAndRejectedFieldRemoval(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.6","max_output_tokens":2048,"input":"hello"}`)
	account := newOpenAIEdgeRetryWSTestAccount(1001, false)
	lease := &openAIEdgeLease{
		edgeRequestID:   "edge-ws-1",
		leaseID:         "lease-ws-1",
		expiresAt:       time.Now().Add(time.Minute),
		account:         account,
		inboundEndpoint: "/v1/responses:ws",
		forwardBody:     append([]byte(nil), body...),
		lastPlan: service.OpenAIEdgePlan{
			EdgeRequestID: "edge-ws-1",
			LeaseID:       "lease-ws-1",
			AccountID:     account.ID,
			Transport:     service.OpenAIEdgeTransportWSV2,
			BodyRawBase64: service.EncodeOpenAIEdgeRawBody(body),
		},
		sameAccountRetries: map[int64]int{},
		sameAccountStarted: map[int64]time.Time{},
		failedAccountIDs:   map[int64]struct{}{},
	}
	h := &OpenAIGatewayHandler{
		gatewayService:   newOpenAIEdgeRetryWSTestService(),
		openAIEdgeLeases: map[string]*openAIEdgeLease{lease.leaseID: lease},
	}
	c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/retry", `{}`, "")

	decision := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{
		LeaseID:            lease.leaseID,
		AccountID:          account.ID,
		UpstreamStatusCode: http.StatusBadRequest,
		ResponseBody:       json.RawMessage(`{"error":{"code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: max_output_tokens"}}`),
	})
	if decision.Action != service.OpenAIEdgeActionRelay || decision.Plan == nil {
		t.Fatalf("expected same-lease WS field retry, got action=%q reason=%q", decision.Action, decision.Reason)
	}
	if gjson.GetBytes(lease.forwardBody, "max_output_tokens").Exists() {
		t.Fatalf("rejected field remained in failover body: %s", lease.forwardBody)
	}

	rebuilt, err := h.buildOpenAIEdgeRetryPlan(c, lease, newOpenAIEdgeRetryWSTestAccount(1002, false), nil)
	if err != nil {
		t.Fatalf("rebuild WS retry plan: %v", err)
	}
	if rebuilt.Transport != service.OpenAIEdgeTransportWSV2 || rebuilt.AccountID != 1002 {
		t.Fatalf("rebuilt retry changed transport/account: transport=%q account=%d", rebuilt.Transport, rebuilt.AccountID)
	}
	rebuiltBody, err := base64.StdEncoding.DecodeString(rebuilt.BodyRawBase64)
	if err != nil {
		t.Fatalf("decode rebuilt WS body: %v", err)
	}
	if gjson.GetBytes(rebuiltBody, "max_output_tokens").Exists() || gjson.GetBytes(rebuiltBody, "type").String() != "response.create" {
		t.Fatalf("rebuilt WS body lost the compatibility rewrite or frame type: %s", rebuiltBody)
	}
	if lease.lastPlan.Transport != service.OpenAIEdgeTransportWSV2 || !bytes.Equal(lease.forwardBody, rebuiltBody) {
		t.Fatal("lease state diverged from the rebuilt WS retry plan")
	}
}

func TestOpenAIEdgeRetryAmbiguousResponses400DoesNotReplay(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","stream":true,"max_output_tokens":2048}`)
	account := &service.Account{ID: 1001}
	h := &OpenAIGatewayHandler{openAIEdgeLeases: map[string]*openAIEdgeLease{
		"lease-1": {
			leaseID:         "lease-1",
			account:         account,
			inboundEndpoint: "/v1/responses",
			lastPlan:        service.OpenAIEdgePlan{BodyRawBase64: service.EncodeOpenAIEdgeRawBody(body)},
		},
	}}
	c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/retry", `{}`, "")
	decision := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{
		LeaseID:            "lease-1",
		AccountID:          account.ID,
		UpstreamStatusCode: http.StatusBadRequest,
		ResponseBody:       json.RawMessage(`{"error":{"code":"invalid_request_error","param":"max_output_tokens","message":"max_output_tokens must be positive"}}`),
	})
	if decision.Action == service.OpenAIEdgeActionRelay {
		t.Fatalf("ambiguous validation error must not replay: %+v", decision)
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
				edgeRequestID: "edge-1",
				leaseID:       "lease-1",
				account:       &service.Account{ID: 1001},
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
	if h.openAIEdgeLeases["lease-1"] == nil {
		t.Fatal("account mismatch must not remove the active lease")
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

func TestOpenAIEdgeSafeErrorMessageRedactsUpstreamIdentityAndCredentials(t *testing.T) {
	message := openAIEdgeSafeErrorMessage("request to https://private.example failed Authorization=Bearer secret-token")
	if message != "Upstream request failed" {
		t.Fatalf("expected generic safe error, got %q", message)
	}
}

func TestOpenAIEdgeSuccessfulTerminalRequiresMatchingDialect(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		terminal string
		want     bool
	}{
		{name: "responses completed", endpoint: "/v1/responses", terminal: "response.completed", want: true},
		{name: "responses incomplete", endpoint: "/v1/responses", terminal: "response.incomplete", want: false},
		{name: "responses done marker", endpoint: "/v1/responses", terminal: "[DONE]", want: false},
		{name: "chat done marker", endpoint: "/v1/chat/completions", terminal: "[DONE]", want: true},
		{name: "chat finish reason", endpoint: "/v1/chat/completions", terminal: "chat.finish_reason", want: true},
		{name: "chat responses terminal", endpoint: "/v1/chat/completions", terminal: "response.completed", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := openAIEdgeSuccessfulTerminal(tt.endpoint, tt.terminal); got != tt.want {
				t.Fatalf("openAIEdgeSuccessfulTerminal() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenAIEdgeCompletionSuccessRejectsCyberAndDisconnect(t *testing.T) {
	base := service.OpenAIEdgeCompleteRequest{
		Success:           true,
		TerminalEventType: "response.completed",
	}
	if !openAIEdgeCompletionIsSuccessful("/v1/responses", base) {
		t.Fatal("completed response should be successful")
	}

	cyber := base
	cyber.CyberBlocked = true
	if openAIEdgeCompletionIsSuccessful("/v1/responses", cyber) {
		t.Fatal("cyber-blocked completion must not be successful")
	}

	disconnected := base
	disconnected.ClientDisconnected = true
	if openAIEdgeCompletionIsSuccessful("/v1/responses", disconnected) {
		t.Fatal("client-disconnected completion must not be successful")
	}
}

func TestOpenAIEdgeCachePolicyCompatibilityFailureIsLeaseScoped(t *testing.T) {
	req := service.OpenAIEdgeCompleteRequest{ErrorType: "cache_creation_optimization_unsupported"}
	if !openAIEdgeCachePolicyCompatibilityFailure(&openAIEdgeLease{cachePolicyEnabled: true}, req) {
		t.Fatal("an applied policy compatibility failure should be health-neutral")
	}
	if openAIEdgeCachePolicyCompatibilityFailure(&openAIEdgeLease{}, req) {
		t.Fatal("a disabled account lease must not inherit compatibility handling")
	}
	if openAIEdgeCachePolicyCompatibilityFailure(&openAIEdgeLease{cachePolicyEnabled: true}, service.OpenAIEdgeCompleteRequest{ErrorType: "upstream_error"}) {
		t.Fatal("ordinary upstream failures must remain health samples")
	}
}

func TestOpenAIEdgeRealFirstTokenMSPrefersRealSample(t *testing.T) {
	perceived := int64(25)
	real := int64(240)
	got := openAIEdgeRealFirstTokenMS(&perceived, &real)
	if got == nil || *got != int(real) {
		t.Fatalf("first token sample = %v, want real sample %d", got, real)
	}

	got = openAIEdgeRealFirstTokenMS(&perceived, nil)
	if got == nil || *got != int(perceived) {
		t.Fatalf("legacy first token sample = %v, want %d", got, perceived)
	}
}

func TestOpenAIEdgeAbortReasonAlreadyRecordedDoesNotReportAnotherFailure(t *testing.T) {
	if !openAIEdgeAbortReasonIsNeutral("retry_failure_already_recorded: max_account_switches_exhausted") {
		t.Fatal("failure-recorded retry abort must not add a second health failure")
	}
	for _, reason := range []string{
		"prepare_failed",
		"ws_prepare_failed",
		"unsupported_ws_transport",
		"ws_proxy_not_supported",
		"edge proxy client capacity exhausted",
		"edge_transient_proxy_client_build_failed",
		"edge_upstream_client_build_failed",
	} {
		if !openAIEdgeAbortReasonIsNeutral(reason) {
			t.Fatalf("local edge abort %q must not penalize account health", reason)
		}
	}
	if openAIEdgeAbortReasonIsNeutral("ordinary_upstream_failure") {
		t.Fatal("ordinary upstream failure must remain a health failure")
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
