package securityaudit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPromptAdminHandlerFailsClosedWhenSystemFeatureIsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := NewService(nil, nil, nil, auditEncryptor{})
	handler := NewPromptAdminHandler(svc)
	router := gin.New()
	router.GET("/config", handler.GetConfig)
	router.POST("/probe", handler.ProbeEndpoint)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/config"},
		{method: http.MethodPost, path: "/probe"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "PROMPT_AUDIT_DISABLED") {
			t.Fatalf("%s %s status=%d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	svc.SetFeatureEnabled(true)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled config status=%d body=%s", rec.Code, rec.Body.String())
	}
}
