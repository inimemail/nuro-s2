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

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// Exercise the actual Rust WS relay and its Go settlement payload. The binary
// is opt-in, matching the existing real-transport recovery test.
func TestEdgeWSRealRustHealthSettlement(t *testing.T) {
	binary := os.Getenv("NURO_EDGE_TEST_BINARY")
	if binary == "" {
		t.Skip("set NURO_EDGE_TEST_BINARY to run the Rust/Go transport integration")
	}
	for _, scenario := range []string{"completed_tcp_close", "completed_idle", "active_tcp_close", "heartbeat_stall", "reasoning_progress", "protection_off", "client_cancel"} {
		t.Run(scenario, func(t *testing.T) {
			upstreamClosed := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer conn.Close() // Intentionally close TCP without a WS Close frame.
				defer close(upstreamClosed)
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, _, err := conn.ReadMessage(); err != nil {
					t.Errorf("first request: %v", err)
					return
				}
				send := func(frame string) bool { return conn.WriteMessage(websocket.TextMessage, []byte(frame)) == nil }
				if !send(`{"type":"response.created","response":{"id":"resp-health"}}`) {
					return
				}
				switch scenario {
				case "active_tcp_close":
					return
				case "client_cancel":
					_, _, _ = conn.ReadMessage()
					return
				case "heartbeat_stall", "reasoning_progress", "protection_off":
					for i := 0; i < 8; i++ {
						time.Sleep(40 * time.Millisecond)
						frame := `{"type":"response.in_progress"}`
						if scenario == "reasoning_progress" {
							frame = `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`
						}
						if !send(frame) {
							return
						}
					}
				}
				send(`{"type":"response.completed","response":{"id":"resp-health","status":"completed","usage":{"input_tokens":7,"output_tokens":3}}}`)
				if scenario == "completed_idle" {
					time.Sleep(450 * time.Millisecond) // Idle sessions must have no generation watchdog.
				}
			}))
			defer upstream.Close()
			completed := make(chan service.OpenAIEdgeCompleteRequest, 4)
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/internal/edge/openai/prepare":
					var req service.OpenAIEdgePrepareRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Errorf("prepare: %v", err)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"action": "relay", "edge_request_id": req.EdgeRequestID, "lease_id": "lease-health", "account_id": 1,
						"transport": "ws_v2", "response_dialect": "responses", "upstream_url": "ws" + strings.TrimPrefix(upstream.URL, "http"),
						"edge_protection_enabled": scenario != "protection_off", "edge_body_idle_timeout_ms": 200,
					})
				case "/internal/edge/openai/complete":
					var req service.OpenAIEdgeCompleteRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Errorf("complete: %v", err)
						return
					}
					completed <- req
					_, _ = io.WriteString(w, `{"ok":true}`)
				default:
					_, _ = io.WriteString(w, `{"ok":true}`)
				}
			}))
			defer control.Close()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			addr := listener.Addr().String()
			require.NoError(t, listener.Close())
			ctx, cancel := context.WithCancel(context.Background())
			cmd := exec.CommandContext(ctx, binary)
			cmd.Env = append(os.Environ(), "SUB2API_EDGE_LISTEN_ADDR="+addr, "SUB2API_EDGE_GO_BASE_URL="+control.URL,
				"SUB2API_EDGE_CONTROL_BASE_URL="+control.URL, "SUB2API_EDGE_INTERNAL_SECRET=test-secret",
				"SUB2API_EDGE_SETTLEMENT_WAL_DIR="+t.TempDir(), "SUB2API_EDGE_UPSTREAM_WARM_URL=",
				"SUB2API_EDGE_LOCAL_DATA_PLANE_FORCE_OFF=true", "SUB2API_EDGE_WS_IDLE_PER_KEY=0")
			var logs bytes.Buffer
			cmd.Stdout, cmd.Stderr = &logs, &logs
			require.NoError(t, cmd.Start())
			defer func() {
				cancel()
				_ = cmd.Wait()
				if t.Failed() {
					t.Log(logs.String())
				}
			}()
			require.Eventually(t, func() bool {
				resp, err := http.Get("http://" + addr + "/healthz")
				if err != nil {
					return false
				}
				_ = resp.Body.Close()
				return resp.StatusCode == http.StatusOK
			}, 5*time.Second, 20*time.Millisecond)
			conn, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/v1/responses", nil)
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-test","input":"hi"}`)))
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			for {
				_, _, err := conn.ReadMessage()
				if err != nil || scenario == "client_cancel" {
					break
				}
			}
			_ = conn.Close()
			select {
			case req := <-completed:
				switch scenario {
				case "active_tcp_close":
					require.False(t, req.Success)
					require.Equal(t, "upstream_disconnect", req.FailureClass)
				case "heartbeat_stall":
					require.False(t, req.Success)
					require.Equal(t, "upstream_no_progress", req.FailureClass)
				case "client_cancel":
					require.True(t, req.ClientDisconnected)
					require.Equal(t, "client_cancelled", req.FailureClass)
				default:
					require.True(t, req.Success, "a completed generation must survive an idle TCP close: %+v", req)
					require.Empty(t, req.FailureClass)
					require.Equal(t, "response.completed", req.TerminalEventType)
					require.Equal(t, 7, req.Usage.InputTokens)
					require.Equal(t, 3, req.Usage.OutputTokens)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("missing completion")
			}
			select {
			case <-upstreamClosed:
			case <-time.After(time.Second):
				t.Fatal("upstream was not closed after settlement")
			}
		})
	}
}
