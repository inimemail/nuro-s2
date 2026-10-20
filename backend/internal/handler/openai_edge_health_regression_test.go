package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestOpenAIEdgeTurnHealthSurvivesConcurrentSettlement(t *testing.T) {
	for _, state := range []string{"active", "settled_reference", "removed", "tampered", "wrong_account"} {
		t.Run(state, func(t *testing.T) {
			h := &OpenAIGatewayHandler{cfg: &config.Config{}, gatewayService: &service.OpenAIGatewayService{}}
			h.cfg.Gateway.OpenAIEdgeRS.InternalSecret = "health-test-secret"
			lease := &openAIEdgeLease{
				leaseID: "lease", edgeRequestID: "request", inboundEndpoint: "/v1/responses:ws",
				apiKey: &service.APIKey{ID: 1, User: &service.User{ID: 2}}, account: &service.Account{ID: 3},
			}
			token, err := h.sealEdgeSettlement(edgeSettlementSnapshot(lease))
			require.NoError(t, err)
			req := service.OpenAIEdgeCompleteRequest{
				LeaseID: lease.leaseID, EdgeRequestID: lease.edgeRequestID, AccountID: lease.account.ID,
				HealthOnly: true, HealthEventID: "request:1", SettlementContext: token, Success: true,
			}
			if state != "removed" {
				h.openAIEdgeLeases = map[string]*openAIEdgeLease{lease.leaseID: lease}
			}
			// The settled pointer models a health callback that obtained the
			// pointer before final completion removed it, then waited on retryMu.
			lease.settled = state != "active"
			if state == "tampered" {
				req.SettlementContext = "invalid"
			}
			if state == "wrong_account" {
				req.AccountID++
			}
			c, w := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/complete", `{}`, "")
			h.openAIEdgeTurnHealth(c, req)
			if state == "tampered" || state == "wrong_account" {
				require.Equal(t, http.StatusConflict, w.Code)
			} else {
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			}
		})
	}
}

func TestEdgeWSRealRustFailedTurnReportsBeforeClientClose(t *testing.T) {
	binary := os.Getenv("NURO_EDGE_TEST_BINARY")
	if binary == "" {
		t.Skip("set NURO_EDGE_TEST_BINARY to run Rust transport integration")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp-audit","model":"gpt-upstream-version"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.failed","response":{"id":"resp-audit","status":"failed","error":{"type":"server_error","code":"server_error","message":"upstream failure"}}}`))
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	complete := make(chan service.OpenAIEdgeCompleteRequest, 4)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/edge/openai/prepare":
			var req service.OpenAIEdgePrepareRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(map[string]any{"action": "relay", "edge_request_id": req.EdgeRequestID, "lease_id": "audit-lease", "account_id": 1, "transport": "ws_v2", "response_dialect": "responses", "upstream_url": "ws" + strings.TrimPrefix(upstream.URL, "http"), "turn_health_enabled": true, "edge_protection_enabled": true, "edge_body_idle_timeout_ms": 200})
		case "/internal/edge/openai/complete":
			var req service.OpenAIEdgeCompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			complete <- req
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer control.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append(os.Environ(), "SUB2API_EDGE_LISTEN_ADDR="+addr, "SUB2API_EDGE_GO_BASE_URL="+control.URL, "SUB2API_EDGE_CONTROL_BASE_URL="+control.URL, "SUB2API_EDGE_INTERNAL_SECRET=test-secret", "SUB2API_EDGE_SETTLEMENT_WAL_DIR="+t.TempDir(), "SUB2API_EDGE_UPSTREAM_WARM_URL=", "SUB2API_EDGE_LOCAL_DATA_PLANE_FORCE_OFF=true", "SUB2API_EDGE_WS_IDLE_PER_KEY=0")
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	require.NoError(t, cmd.Start())
	defer func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	require.Eventually(t, func() bool {
		r, e := http.Get("http://" + addr + "/healthz")
		if e != nil {
			return false
		}
		_ = r.Body.Close()
		return r.StatusCode == 200
	}, 5*time.Second, 20*time.Millisecond)
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/v1/responses", nil)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-test","input":"hi"}`)))
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		_, frame, e := conn.ReadMessage()
		require.NoError(t, e)
		if strings.Contains(string(frame), "response.failed") {
			break
		}
	}
	select {
	case req := <-complete:
		require.True(t, req.HealthOnly)
		require.False(t, req.Success)
		require.False(t, req.ClientDisconnected)
		require.Equal(t, "upstream_error", req.FailureClass)
		require.Equal(t, "gpt-test", req.Model, "health must follow the requested route, even when the provider returns a different model name")
	case <-time.After(3 * time.Second):
		t.Fatal("failed WS turn was not reported while session remained open")
	}

	_ = conn.Close()
	select {
	case req := <-complete:
		t.Logf("after explicit upstream response.failed then client close: success=%v class=%q client_disconnected=%v terminal=%q", req.Success, req.FailureClass, req.ClientDisconnected, req.TerminalEventType)
		require.True(t, req.ClientDisconnected)
		require.Equal(t, "upstream_error", req.FailureClass)
		require.True(t, req.TurnHealthReported)
		require.Equal(t, "response.failed", req.TerminalEventType)
		require.Equal(t, "gpt-upstream-version", req.Model, "billing must retain the upstream response model")
	case <-time.After(4 * time.Second):
		t.Fatal("missing completion")
	}
}

func TestOpenAIEdgeSSEExplicitFailureUsesSemanticClassification(t *testing.T) {
	for _, scenario := range []struct {
		name, payload, message string
		userError              bool
	}{
		{"rate_limit", `{"type":"response.failed","response":{"error":{"status":429,"type":"rate_limit_error","code":"rate_limit_exceeded","message":"rate limit reached"}}}`, "rate limit reached", false},
		{"invalid_input", `{"type":"response.failed","response":{"error":{"status":400,"type":"invalid_request_error","code":"invalid_request","message":"input must be a string"}}}`, "input must be a string", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			gateway := &service.OpenAIGatewayService{}
			account := &service.Account{ID: 905, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_enabled": true, "pool_soft_cooldown_error_threshold": 1}}
			semantic, should := gateway.ClassifyOpenAIEdgeStreamFailure(context.Background(), account, []byte(scenario.payload), scenario.message)
			if scenario.userError {
				require.False(t, should)
			} else {
				require.True(t, should)
				require.Equal(t, 429, semantic.StatusCode)
			}
			lease := &openAIEdgeLease{leaseID: "audit-semantic", account: account, apiKey: &service.APIKey{}, expiresAt: time.Now().Add(time.Minute)}
			h := &OpenAIGatewayHandler{gatewayService: gateway, openAIEdgeLeases: map[string]*openAIEdgeLease{lease.leaseID: lease}}
			c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/retry", `{}`, "")
			decision := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{LeaseID: lease.leaseID, AccountID: account.ID, ErrorType: "edge_upstream_stream_failed", ErrorMessage: scenario.message, ResponseBody: json.RawMessage(scenario.payload), ExecutionState: "unknown"})
			require.NotEqual(t, "execution_unknown_replay_blocked", decision.Reason)
			t.Logf("semantic_should_failover=%v actual_reason=%q recorded_failure=%v pool_cooling=%v", should, decision.Reason, decision.FailureRecorded, gateway.OpenAIPoolSoftCooldownState(account.ID).Cooling)
			require.False(t, decision.FailureRecorded)
			require.False(t, gateway.OpenAIPoolSoftCooldownState(account.ID).Cooling)
		})
	}
}

func TestOpenAIEdgeServerFailureIsRetainedAfterClientDisconnect(t *testing.T) {
	for _, disconnected := range []bool{false, true} {
		req := service.OpenAIEdgeCompleteRequest{FailureClass: "upstream_error", UpstreamStatusCode: 502,
			TerminalEventType: "response.failed", ClientDisconnected: disconnected}
		require.True(t, openAIEdgeConfirmedAccountFailure(req))
		require.True(t, openAIEdgeShouldRecordCircuitOutcome(req, false, false))
		req.UpstreamStatusCode = 400
		require.False(t, openAIEdgeShouldRecordCircuitOutcome(req, false, false), "request errors must not trigger transport avoidance")
		req.UpstreamStatusCode = 502
		req.TerminalEventType = "response.incomplete"
		require.False(t, openAIEdgeShouldRecordCircuitOutcome(req, false, false))
	}
}
