package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestOpenAIEdgeCommitReleasesRetryPayloadWithoutSettlingLease(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{
		cfg: &config.Config{},
		openAIEdgeLeases: map[string]*openAIEdgeLease{
			"lease-1": {
				edgeRequestID: "edge-1",
				leaseID:       "lease-1",
				account:       &service.Account{ID: 7},
				forwardBody:   []byte(`{"model":"gpt-5.6"}`),
				lastPlan:      service.OpenAIEdgePlan{BodyRawBase64: "payload"},
			},
		},
		openAIEdgeLeaseByRequest: map[string]string{"edge-1": "lease-1"},
	}
	h.cfg.Gateway.OpenAIEdgeRS.InternalAPIEnabled = true
	h.cfg.Gateway.OpenAIEdgeRS.InternalSecret = "secret"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/edge/openai/commit", bytes.NewBufferString(`{"edge_request_id":"edge-1","lease_id":"lease-1","account_id":7}`))
	c.Request.Header.Set(openAIEdgeSecretHeader, "secret")
	h.OpenAIEdgeCommit(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var ack service.OpenAIEdgeAck
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil || !ack.OK {
		t.Fatalf("ack=%s err=%v", w.Body.String(), err)
	}
	lease := h.openAIEdgeLeases["lease-1"]
	if lease == nil || lease.settled || len(lease.forwardBody) != 0 || lease.lastPlan.BodyRawBase64 != "" || !lease.payloadReleased {
		t.Fatal("commit did not preserve active lease while releasing payload")
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/edge/openai/commit", bytes.NewBufferString(`{"edge_request_id":"edge-1","lease_id":"lease-1","account_id":7}`))
	c.Request.Header.Set(openAIEdgeSecretHeader, "secret")
	h.OpenAIEdgeCommit(c)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("payload_already_released")) {
		t.Fatalf("idempotent commit=%d %s", w.Code, w.Body.String())
	}
}

func TestOpenAIEdgeRenewExtendsShortCrashRecoveryLease(t *testing.T) {
	gin.SetMode(gin.TestMode)
	released := make(chan struct{}, 1)
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{
		InternalAPIEnabled: true,
		InternalSecret:     "secret",
		LeaseTTLMS:         200,
	}
	now := time.Now()
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-renew",
		leaseID:       "lease-renew",
		createdAt:     now,
		expiresAt:     now.Add(200 * time.Millisecond),
		account:       &service.Account{ID: 7},
		accountReleaseFunc: func() {
			released <- struct{}{}
		},
	}
	if !h.storeOpenAIEdgeLease(lease, 200*time.Millisecond) {
		t.Fatal("failed to store lease")
	}
	time.Sleep(130 * time.Millisecond)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/edge/openai/renew", bytes.NewBufferString(`{"edge_request_id":"edge-renew","lease_id":"lease-renew","account_id":7}`))
	c.Request.Header.Set(openAIEdgeSecretHeader, "secret")
	h.OpenAIEdgeRenew(c)
	if w.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", w.Code, w.Body.String())
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-released:
		t.Fatal("lease expired at its original deadline after renewal")
	default:
	}
	select {
	case <-released:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("renewed lease was not reclaimed after heartbeat stopped")
	}
}

func TestOpenAIEdgeCompleteReleasesLeaseImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	released := make(chan struct{}, 1)
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{
		InternalAPIEnabled: true,
		InternalSecret:     "secret",
		LeaseTTLMS:         1000,
	}
	now := time.Now()
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-complete",
		leaseID:       "lease-complete",
		createdAt:     now,
		expiresAt:     now.Add(time.Second),
		account:       &service.Account{ID: 8},
		accountReleaseFunc: func() {
			released <- struct{}{}
		},
	}
	if !h.storeOpenAIEdgeLease(lease, time.Second) {
		t.Fatal("failed to store lease")
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/edge/openai/complete", bytes.NewBufferString(`{"edge_request_id":"edge-complete","lease_id":"lease-complete","account_id":8,"success":false}`))
	c.Request.Header.Set(openAIEdgeSecretHeader, "secret")
	h.OpenAIEdgeComplete(c)
	if w.Code != http.StatusOK {
		t.Fatalf("complete status=%d body=%s", w.Code, w.Body.String())
	}
	select {
	case <-released:
	default:
		t.Fatal("complete returned before releasing account slot")
	}
	lease.mu.Lock()
	timer := lease.timer
	lease.mu.Unlock()
	if timer != nil {
		t.Fatal("complete retained the lease expiry timer")
	}
}
