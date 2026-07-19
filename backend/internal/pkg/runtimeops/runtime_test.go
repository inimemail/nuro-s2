package runtimeops

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestDrainAllowsInternalSettlementButRejectsPublicTraffic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	process = newProcessState()
	SetDraining(true)
	r := gin.New()
	r.Use(Middleware())
	r.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/internal/edge/openai/complete", func(c *gin.Context) { c.Status(http.StatusOK) })

	public := httptest.NewRecorder()
	r.ServeHTTP(public, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if public.Code != http.StatusServiceUnavailable {
		t.Fatalf("public status=%d", public.Code)
	}
	internal := httptest.NewRecorder()
	r.ServeHTTP(internal, httptest.NewRequest(http.MethodPost, "/internal/edge/openai/complete", nil))
	if internal.Code != http.StatusOK {
		t.Fatalf("internal status=%d", internal.Code)
	}
}

func TestCurrentIncludesAdmissionMetrics(t *testing.T) {
	process = newProcessState()
	ObserveAdmissionClaim(2*time.Millisecond, nil)
	ObserveAdmissionClaim(20*time.Millisecond, errors.New("claim"))
	snapshot := Current()
	if snapshot.AdmissionClaims != 2 || snapshot.AdmissionErrors != 1 || snapshot.AdmissionMicros == 0 {
		t.Fatalf("admission metrics not captured: %+v", snapshot)
	}
}
