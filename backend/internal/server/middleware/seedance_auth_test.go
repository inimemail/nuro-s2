//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestV027SeedanceManagementAuthKeepsSecurityAndSkipsNewPurchaseChecks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, prefix := range []string{"/api/v3", "/v3", "/v1", ""} {
		for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPost} {
			for _, scenario := range []string{"exhausted", "disabled", "inactive_user", "missing_key"} {
				t.Run(prefix+"/"+method+"/"+scenario, func(t *testing.T) {
					expiredAt := time.Now().Add(-time.Hour)
					key := &service.APIKey{ID: 2, UserID: 1, Key: "test-only", Status: service.StatusAPIKeyQuotaExhausted, Quota: 1, QuotaUsed: 1, ExpiresAt: &expiredAt, User: &service.User{ID: 1, Status: service.StatusActive, Balance: 0}}
					if scenario == "disabled" {
						key.Status = service.StatusDisabled
					}
					if scenario == "inactive_user" {
						key.User.Status = service.StatusDisabled
					}
					repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { clone := *key; return &clone, nil }, updateLastUsed: func(context.Context, int64, time.Time) error { return nil }}
					cfg := &config.Config{RunMode: config.RunModeStandard}
					keys := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
					router := gin.New()
					router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, nil, cfg)))
					path := prefix + "/contents/generations/tasks"
					if method != http.MethodPost {
						path += "/task1"
					}
					router.Handle(method, path, func(c *gin.Context) { c.Status(http.StatusOK) })
					req := httptest.NewRequest(method, path, nil)
					if scenario != "missing_key" {
						req.Header.Set("Authorization", "Bearer test-only")
					}
					w := httptest.NewRecorder()
					router.ServeHTTP(w, req)
					if scenario == "exhausted" && method != http.MethodPost {
						require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					} else {
						require.GreaterOrEqual(t, w.Code, 400, w.Body.String())
					}
				})
			}
		}
	}
}

func TestV027SeedanceBillingExemptionIsExact(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/contents/generations/tasks", "/v1/contents/generations/tasks/", "/v1/contents/generations/tasks/../responses", "/v1/contents/generations/tasks/task1/extra"} {
		require.False(t, isSeedanceTaskManagement(http.MethodGet, path), path)
	}
	require.False(t, isSeedanceTaskManagement(http.MethodPost, "/v1/contents/generations/tasks/task1"))
}
