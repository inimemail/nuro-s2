//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIEdgeDeliveredPOSTIsNeverReplayed(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		received <- string(b)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer server.Close()
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{Enabled: true, InternalAPIEnabled: true, InternalSecret: "test", IngressProxyEnabled: true, Mode: "relay", ListenAddr: server.URL}
	body := `{"model":"gpt-test","input":"hi","stream":true}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
	require.True(t, h.tryOpenAIEdgeIngressProxy(c), "must not fall through to a second execution")
	require.Equal(t, body, <-received)
	require.Equal(t, http.StatusBadGateway, w.Code)
	server.Close()
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
	require.False(t, h.tryOpenAIEdgeIngressProxy(c), "connection refusal before sending can still use Go")
	b, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(b))
}

func TestOpenAIEdgeTerminalMustBeCompleteSSEFrame(t *testing.T) {
	for _, text := range []string{"event: response.completed", "data: [DONE]", `{"type":"response.completed"}`} {
		data, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": text})
		body := "data: " + string(data) + "\n\n"
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		copyOpenAIEdgeResponseBody(c, &edgeOneByteReader{Reader: strings.NewReader(body)}, true)
		require.True(t, strings.HasPrefix(w.Body.String(), body))
		require.Contains(t, w.Body.String(), "event: response.failed\n")
	}
	for _, eol := range []string{"\n", "\r\n", "\r"} {
		body := "event: response.completed" + eol + "data: {\"type\":" + eol + "data: \"response.completed\"}" + eol + eol
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		copyOpenAIEdgeResponseBody(c, &edgeOneByteReader{Reader: strings.NewReader(body)}, true)
		require.Equal(t, body, w.Body.String())
	}
	s := openAIEdgeTerminalScanner{responses: true}
	s.feed([]byte("data: {\"type\":\"response.completed\"}\n"))
	require.False(t, s.seen, "unterminated frame is not a terminal")
	s.feed([]byte("\n"))
	require.True(t, s.seen)
	for _, invalid := range []string{"event: response.completed\n\n", "event: response.completed\ndata: {\n\n", "data: {\"response\":{\"type\":\"response.completed\"}}\n\n"} {
		invalidScanner := openAIEdgeTerminalScanner{responses: true}
		invalidScanner.feed([]byte(invalid))
		require.False(t, invalidScanner.seen)
	}
	s = openAIEdgeTerminalScanner{responses: true}
	s.feed(bytes.Repeat([]byte("x"), (16<<20)+1))
	s.feed([]byte("\nevent: response.completed\n\ndata: {\"type\":\"response.output_text.delta\"}\n\n"))
	require.False(t, s.seen, "oversized frame must be discarded up to its actual blank line")
	s.feed([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	require.True(t, s.seen)
}

func TestOpenAIEdgeBusyLeaseDoesNotBlockRegistry(t *testing.T) {
	for _, operation := range []string{"renew", "take", "cancel", "commit", "expire", "recover"} {
		t.Run(operation, func(t *testing.T) {
			a := &openAIEdgeLease{leaseID: "a", edgeRequestID: "ra", edgeNodeID: "node", edgeInstanceID: "old", expiresAt: time.Now().Add(-time.Second)}
			b := &openAIEdgeLease{leaseID: "b", edgeRequestID: "rb"}
			h := &OpenAIGatewayHandler{openAIEdgeLeases: map[string]*openAIEdgeLease{"a": a, "b": b}, openAIEdgeLeaseByRequest: map[string]string{"ra": "a", "rb": "b"}}
			a.retryMu.Lock()
			done := make(chan struct{})
			go func() {
				defer close(done)
				switch operation {
				case "renew":
					h.renewOpenAIEdgeLeaseForRequest("a", "ra", 0, time.Minute)
				case "take":
					h.takeOpenAIEdgeLeaseForRequest("a", "ra", 0, false)
				case "cancel":
					h.cancelOpenAIEdgeLeaseForRequest("a", "ra", 0, time.Minute)
				case "commit":
					h.releaseOpenAIEdgeRetryPayload("a", "ra", 0)
				case "expire":
					h.expireOpenAIEdgeLease(a, 0)
				case "recover":
					h.recoverOpenAIEdgeLeases("node", "new")
				}
			}()
			time.Sleep(10 * time.Millisecond)
			other := make(chan *openAIEdgeLease, 1)
			go func() { other <- h.getOpenAIEdgeLease("b") }()
			select {
			case actual := <-other:
				require.Same(t, b, actual)
			case <-time.After(time.Second):
				a.retryMu.Unlock()
				<-done
				t.Fatal("unrelated lease blocked")
			}
			a.retryMu.Unlock()
			<-done
			a.release()
		})
	}
}

func TestOpenAIEdgeSettlementSurvivesLostLeaseAndRejectsTampering(t *testing.T) {
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{InternalAPIEnabled: true, InternalSecret: "test"}
	l := &openAIEdgeLease{leaseID: "lease", edgeRequestID: "request", requestModel: "gpt-4.1", billingModel: "gpt-4.1", inboundEndpoint: "/v1/responses", requestPayloadHash: "payload-hash",
		apiKey: &service.APIKey{ID: 1, User: &service.User{ID: 2, PasswordHash: "private-hash"}}, account: &service.Account{ID: 3, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "private-upstream-key"}}}
	token, err := h.sealEdgeSettlement(edgeSettlementSnapshot(l))
	require.NoError(t, err)
	require.NotContains(t, token, "private")
	req := service.OpenAIEdgeCompleteRequest{LeaseID: "lease", EdgeRequestID: "request", AccountID: 3, SettlementContext: token}
	other := &OpenAIGatewayHandler{cfg: h.cfg}
	restored, err := other.restoreEdgeSettlement(req)
	require.NoError(t, err)
	require.Equal(t, l.requestPayloadHash, restored.requestPayloadHash)
	require.Equal(t, l.account.ID, restored.account.ID)
	require.Empty(t, restored.apiKey.User.PasswordHash)
	req.AccountID++
	_, err = other.restoreEdgeSettlement(req)
	require.Error(t, err)
	req.AccountID--
	raw, err := base64.RawURLEncoding.DecodeString(token)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 1
	req.SettlementContext = base64.RawURLEncoding.EncodeToString(raw)
	_, err = other.restoreEdgeSettlement(req)
	require.Error(t, err)
	c, w := newOpenAIEdgeTestContext("POST", "/internal/edge/openai/complete", `{"lease_id":"missing","edge_request_id":"missing","usage":{"input_tokens":100}}`, "test")
	other.OpenAIEdgeComplete(c)
	require.Equal(t, http.StatusConflict, w.Code)
}

type edgeSettlementBillingStub struct {
	service.UsageBillingRepository
	commands map[string]string
	fail     bool
	applied  int
}

func (s *edgeSettlementBillingStub) Apply(_ context.Context, c *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	if s.fail {
		return nil, context.DeadlineExceeded
	}
	c.Normalize()
	if fingerprint, ok := s.commands[c.RequestID]; ok {
		if fingerprint != c.RequestFingerprint {
			return nil, service.ErrUsageBillingRequestConflict
		}
		return &service.UsageBillingApplyResult{Applied: false}, nil
	}
	s.commands[c.RequestID] = c.RequestFingerprint
	s.applied++
	// Simulate the DB transaction committed but its acknowledgement was lost.
	return nil, context.DeadlineExceeded
}

func TestOpenAIEdgeRecoveredCompletionBillsIdempotently(t *testing.T) {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{InternalAPIEnabled: true, InternalSecret: "test"}
	billing := &edgeSettlementBillingStub{commands: make(map[string]string), fail: true}
	logs := &openAIWSUsageHandlerUsageLogRepoStub{}
	svc := service.NewOpenAIGatewayService(nil, logs, billing, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h := &OpenAIGatewayHandler{cfg: cfg, gatewayService: svc}
	l := &openAIEdgeLease{leaseID: "lease", edgeRequestID: "request", requestModel: "gpt-4.1", billingModel: "gpt-4.1", inboundEndpoint: "/v1/responses", requestPayloadHash: "hash",
		apiKey: &service.APIKey{ID: 1, UserID: 2, User: &service.User{ID: 2}}, account: &service.Account{ID: 3, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}}
	releases := 0
	l.accountReleaseFunc = func() { releases++ }
	l.expiresAt = time.Now().Add(time.Minute)
	require.True(t, h.storeOpenAIEdgeLease(l, time.Minute))
	t.Cleanup(l.release)
	token, err := h.sealEdgeSettlement(edgeSettlementSnapshot(l))
	require.NoError(t, err)
	req := service.OpenAIEdgeCompleteRequest{LeaseID: l.leaseID, EdgeRequestID: l.edgeRequestID, AccountID: 3, SettlementContext: token, FailureClass: "lease_lost", TerminalEventType: "response.incomplete", Usage: service.OpenAIUsage{InputTokens: 100, OutputTokens: 30}}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	complete := func() int {
		c, w := newOpenAIEdgeTestContext("POST", "/internal/edge/openai/complete", string(body), "test")
		h.OpenAIEdgeComplete(c)
		return w.Code
	}
	require.Equal(t, http.StatusServiceUnavailable, complete(), "failed transaction must remain in Edge WAL")
	require.Equal(t, 1, releases, "stream completion releases its slot even when billing needs a retry")
	h = &OpenAIGatewayHandler{cfg: cfg, gatewayService: svc}
	billing.fail = false
	require.Equal(t, http.StatusOK, complete())
	// A provider can omit its ID on an interrupted callback, then report it
	// later. Both must retain the original authenticated billing identity.
	req.RequestID = "late-provider-request-id"
	body, err = json.Marshal(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, complete())
	require.Equal(t, 1, billing.applied, "lost ack and repeated callback must not deduct twice")
	require.Contains(t, billing.commands, "edge-settlement:lease")
	// A pricing change between a committed transaction and delayed replay is
	// not a transient DB outage. Keep it for reconciliation, never recharge.
	cfg.Default.RateMultiplier = 2
	require.Equal(t, http.StatusConflict, complete())
	require.Equal(t, 1, billing.applied)
	cfg.Default.RateMultiplier = 1
	require.Equal(t, http.StatusOK, complete())
	req.Usage.OutputTokens++
	body, err = json.Marshal(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, complete(), "changed usage must not silently pass deduplication")
	require.Equal(t, 1, billing.applied)
}

func TestOpenAIEdgeRenewCannotResurrectExpiredLease(t *testing.T) {
	l := &openAIEdgeLease{leaseID: "lease", edgeRequestID: "req", expiresAt: time.Now().Add(-time.Second)}
	h := &OpenAIGatewayHandler{openAIEdgeLeases: map[string]*openAIEdgeLease{"lease": l}, openAIEdgeLeaseByRequest: map[string]string{"req": "lease"}}
	require.Equal(t, "lease_expired", h.renewOpenAIEdgeLeaseForRequest("lease", "req", 0, time.Minute))
	require.True(t, l.expiresAt.Before(time.Now()))
}
