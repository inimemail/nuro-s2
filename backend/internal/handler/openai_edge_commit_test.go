package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
