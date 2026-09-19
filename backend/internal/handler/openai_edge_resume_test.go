package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestEdgeStreamResumesDeliveredPOSTAndPartialFrames(t *testing.T) {
	for _, split := range []int{0, 1, 55, 120} {
		t.Run(strconv.Itoa(split), func(t *testing.T) {
			output := "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"pwd\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"
			var posts, resumes, cancelled atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "test-secret", r.Header.Get(openAIEdgeSecretHeader))
				require.Len(t, r.Header.Get(edgeStreamBindingHeader), 64)
				switch r.Method {
				case http.MethodPut:
					w.WriteHeader(204)
				case http.MethodDelete:
					cancelled.Add(1)
					w.WriteHeader(204)
				case http.MethodPost:
					posts.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					conn, rw, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					if split > 0 {
						_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", split, output[:split])
						_ = rw.Flush()
					}
					_ = conn.Close() // accepted POST or SSE interrupted inside a frame
				case http.MethodGet:
					resumes.Add(1)
					require.Equal(t, strconv.Itoa(split), r.Header.Get(edgeStreamOffsetHeader))
					w.Header().Set(edgeStreamSessionHeader, "v1")
					w.Header().Set(edgeStreamOffsetHeader, strconv.Itoa(split))
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, output[split:])
				}
			}))
			defer server.Close()
			h := &OpenAIGatewayHandler{cfg: &config.Config{}}
			h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{Enabled: true, InternalAPIEnabled: true, InternalSecret: "test-secret", IngressProxyEnabled: true, Mode: "relay", ListenAddr: server.URL}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi","stream":true}`))
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
			require.True(t, h.tryOpenAIEdgeIngressProxy(c))
			require.Equal(t, 200, w.Code)
			require.Equal(t, output, w.Body.String(), "same downstream response, no duplicated tool bytes")
			require.EqualValues(t, 1, posts.Load())
			require.EqualValues(t, 1, resumes.Load())
			require.EqualValues(t, 1, cancelled.Load())
		})
	}
}

// Run with NURO_EDGE_TEST_BINARY pointing to a freshly built local Edge. This
// uses the actual Rust router/relay/journal and real TCP disconnects, with only
// the upstream and control database boundary replaced by local fixtures.
func TestEdgeStreamRealRustTransportRecovery(t *testing.T) {
	binary := os.Getenv("NURO_EDGE_TEST_BINARY")
	if binary == "" {
		t.Skip("set NURO_EDGE_TEST_BINARY to run the Rust/Go transport integration")
	}
	for _, split := range []int{0, 17, 90, -1, -2, -3} {
		t.Run(strconv.Itoa(split), func(t *testing.T) {
			var executions, preparations, settlements atomic.Int32
			usage := make(chan service.OpenAIEdgeCompleteRequest, 10)
			output := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-fixture\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-fixture\",\"model\":\"gpt-test\",\"status\":\"completed\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20}}}\n\n"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Existing Edge connection keep-warm uses GET after 30s. Count
				// generation POSTs, not the unrelated idle-connection probe.
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				attempt := executions.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if split == -3 {
					input, outputTokens := 100, 20
					if attempt > 1 {
						input, outputTokens = 50, 7
					}
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"attempt-%d\",\"usage\":{\"input_tokens\":%d,\"output_tokens\":%d}}}\n\n", attempt, input, outputTokens)
					w.(http.Flusher).Flush()
					if attempt == 1 {
						return // Retry an upstream EOF before real output.
					}
					_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"cancel-after-retry\"}\n\n")
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
					case <-time.After(10 * time.Second):
					}
					return
				}
				_, _ = io.WriteString(w, output[:105])
				w.(http.Flusher).Flush()
				time.Sleep(60 * time.Millisecond)
				_, _ = io.WriteString(w, output[105:])
			}))
			defer upstream.Close()
			plan := func(requestID string) map[string]any {
				return map[string]any{"action": "relay", "edge_request_id": requestID, "lease_id": "lease-fixture", "account_id": 1, "account_type": "apikey", "transport": "http2_sse", "response_dialect": "responses", "upstream_url": upstream.URL, "headers": map[string]string{"Content-Type": "application/json"}, "body": map[string]any{"stream": true}, "lease_ttl_ms": 120000, "lease_renew_grace_ms": 120000, "preamble_flush": true}
			}
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/internal/edge/openai/prepare":
					preparations.Add(1)
					var req service.OpenAIEdgePrepareRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					_ = json.NewEncoder(w).Encode(plan(req.EdgeRequestID))
				case "/internal/edge/openai/retry":
					var req service.OpenAIEdgeRetryRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					_ = json.NewEncoder(w).Encode(map[string]any{"action": "relay", "plan": plan(req.EdgeRequestID)})
				case "/internal/edge/openai/complete":
					var req service.OpenAIEdgeCompleteRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					settlements.Add(1)
					usage <- req
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
			cmd.Env = append(os.Environ(), "SUB2API_EDGE_LISTEN_ADDR="+addr, "SUB2API_EDGE_GO_BASE_URL="+control.URL, "SUB2API_EDGE_CONTROL_BASE_URL="+control.URL, "SUB2API_EDGE_INTERNAL_SECRET=test-secret", "SUB2API_EDGE_SETTLEMENT_WAL_DIR="+t.TempDir(), "SUB2API_EDGE_UPSTREAM_WARM_URL=", "SUB2API_EDGE_LOCAL_DATA_PLANE_FORCE_OFF=true", "SUB2API_EDGE_INITIAL_POOL_SIZE=1")
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
				resp, e := http.Get("http://" + addr + "/healthz")
				if e != nil {
					return false
				}
				_ = resp.Body.Close()
				return resp.StatusCode == 200
			}, 10*time.Second, 20*time.Millisecond)
			fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := r.Clone(r.Context())
				req.URL.Scheme = "http"
				req.URL.Host = addr
				req.RequestURI = ""
				resp, e := http.DefaultTransport.RoundTrip(req)
				if e != nil {
					w.WriteHeader(502)
					return
				}
				defer resp.Body.Close()
				if r.Method == http.MethodPost {
					if split == -1 {
						// Lost initial response: wait for Go's internal header
						// deadline, while the Rust execution completes normally.
						<-r.Context().Done()
						return
					}
					prefixSize := split
					if split == -2 {
						prefixSize = 90
					}
					prefix := make([]byte, prefixSize)
					_, e = io.ReadFull(resp.Body, prefix)
					require.NoError(t, e)
					if split == -2 {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write(prefix)
						w.(http.Flusher).Flush()
						<-r.Context().Done() // blackholed body, no FIN/reset
						return
					}
					conn, rw, e := w.(http.Hijacker).Hijack()
					require.NoError(t, e)
					if split > 0 {
						_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", split, prefix)
						_ = rw.Flush()
					}
					_ = conn.Close()
					return
				}
				for k, v := range resp.Header {
					w.Header()[k] = v
				}
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
			}))
			defer fault.Close()
			h := &OpenAIGatewayHandler{cfg: &config.Config{}}
			h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{Enabled: true, InternalAPIEnabled: true, InternalSecret: "test-secret", IngressProxyEnabled: true, Mode: "relay", ListenAddr: fault.URL}
			w := httptest.NewRecorder()
			requestCtx, requestCancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer requestCancel()
			var writer http.ResponseWriter = w
			if split == -3 {
				h.cfg.Gateway.OpenAIEdgeRS.ListenAddr = "http://" + addr
				writer = &edgeCancelAfterRetryWriter{ResponseRecorder: w, cancel: requestCancel}
			}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi","stream":true}`)).WithContext(requestCtx)
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
			require.True(t, h.tryOpenAIEdgeIngressProxy(c))
			require.Equal(t, 200, w.Code)
			if split == -3 {
				require.Contains(t, w.Body.String(), "cancel-after-retry")
				require.EqualValues(t, 2, executions.Load())
			} else {
				require.Equal(t, output, w.Body.String())
				require.EqualValues(t, 1, executions.Load())
			}
			require.EqualValues(t, 1, preparations.Load())
			select {
			case req := <-usage:
				if split == -3 {
					require.EqualValues(t, 150, req.Usage.InputTokens, "cancellation must preserve both attempts")
					require.EqualValues(t, 27, req.Usage.OutputTokens)
					require.True(t, req.ClientDisconnected)
				} else {
					require.EqualValues(t, 100, req.Usage.InputTokens)
					require.EqualValues(t, 20, req.Usage.OutputTokens)
					require.False(t, req.ClientDisconnected)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no usage settlement")
			}
			require.EqualValues(t, 1, settlements.Load())
		})
	}
}

type edgeCancelAfterRetryWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *edgeCancelAfterRetryWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if strings.Contains(w.Body.String(), "cancel-after-retry") {
		w.cancel()
	}
	return n, err
}

func TestEdgeStreamMissingSessionNeverReexecutes(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut, http.MethodDelete:
			w.WriteHeader(204)
		case http.MethodGet:
			w.WriteHeader(http.StatusGone)
		case http.MethodPost:
			posts.Add(1)
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}
	}))
	defer server.Close()
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{Enabled: true, InternalAPIEnabled: true, InternalSecret: "test-secret", IngressProxyEnabled: true, Mode: "relay", ListenAddr: server.URL}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"stream":true}`))
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
	require.True(t, h.tryOpenAIEdgeIngressProxy(c))
	require.Equal(t, 502, w.Code)
	require.EqualValues(t, 1, posts.Load())
}

func TestEdgeLeaseGraceRetainsConcurrencyAndAllowsRenewal(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS.InternalSecret = "test"
	var releases atomic.Int32
	l := &openAIEdgeLease{leaseID: "grace", edgeRequestID: "request", expiresAt: time.Now().Add(-time.Second),
		apiKey: &service.APIKey{ID: 1, User: &service.User{ID: 1}}, account: &service.Account{ID: 1},
		accountReleaseFunc: func() { releases.Add(1) }}
	require.True(t, h.storeOpenAIEdgeLease(l, time.Minute))
	t.Cleanup(l.release)
	require.Equal(t, 120000, l.lastPlan.LeaseRenewGraceMS)
	h.expireOpenAIEdgeLease(l, l.timerGeneration)
	require.Zero(t, releases.Load(), "grace must retain the actual concurrency release callback")
	require.Empty(t, h.renewOpenAIEdgeLeaseForRequest("grace", "request", 0, time.Minute))
	require.Greater(t, time.Until(l.expiresAt), 2*time.Minute)
	cancelled, reason := h.cancelOpenAIEdgeLeaseForRequest("grace", "request", 0, time.Minute)
	require.Empty(t, reason)
	require.NotNil(t, cancelled)
	cancelled.release() // abort handler releases outside lifecycle locks
	require.EqualValues(t, 1, releases.Load(), "explicit cancellation must still release immediately")
}

func TestEdgeRetryLeaseWindowNeverExtendsExistingOwnership(t *testing.T) {
	for _, remaining := range []time.Duration{240 * time.Second, 121 * time.Second, 120 * time.Second, 40 * time.Second, time.Second} {
		var plan service.OpenAIEdgePlan
		setOpenAIEdgePlanLeaseWindow(&plan, remaining, 120*time.Second)
		require.LessOrEqual(t, time.Duration(plan.LeaseTTLMS+plan.LeaseRenewGraceMS)*time.Millisecond, remaining)
		require.Positive(t, plan.LeaseTTLMS)
	}
}

func TestEdgeFirstSendRejectedBeforeExecutionCanUseGo(t *testing.T) {
	original := openAIEdgeIngressClient
	t.Cleanup(func() { openAIEdgeIngressClient = original })
	posts := 0
	openAIEdgeIngressClient = &http.Client{Transport: grokEdgeTestTransport(func(r *http.Request) (*http.Response, error) {
		status := http.StatusNoContent
		headers := make(http.Header)
		if r.Method == http.MethodPost {
			posts++
			status = http.StatusGone
			headers.Set(edgeStreamSessionHeader, "absent")
		}
		return &http.Response{StatusCode: status, Header: headers, Body: http.NoBody}, nil
	})}
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{Enabled: true, InternalAPIEnabled: true, InternalSecret: "test", IngressProxyEnabled: true, Mode: "relay", ListenAddr: "http://edge.test"}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"stream":true,"input":"hi"}`
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
	require.False(t, h.tryOpenAIEdgeIngressProxy(c))
	require.False(t, c.Writer.Written())
	got, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(got))
	require.Equal(t, 1, posts)
}

func TestEdgeResumeNeverRestartsAfterAnyDeliveredBytes(t *testing.T) {
	original := openAIEdgeIngressClient
	t.Cleanup(func() { openAIEdgeIngressClient = original })
	openAIEdgeIngressClient = &http.Client{Transport: grokEdgeTestTransport(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodGet, r.Method)
		return &http.Response{StatusCode: http.StatusTooEarly, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	req, _ := http.NewRequest(http.MethodPost, "http://edge.test/v1/responses", nil)
	s := &edgeStreamSession{url: "http://edge.test/session", request: req}
	_, err := s.resume(10)
	require.ErrorContains(t, err, "lost execution state")
}
