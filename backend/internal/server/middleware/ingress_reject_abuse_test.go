package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type invalidAuthLimiterStub struct {
	blocked  map[string]bool
	recorded []string
}

func (s *invalidAuthLimiterStub) CheckInvalidAuthAbuse(key string) (time.Duration, bool) {
	return time.Second, s.blocked[key]
}

func (s *invalidAuthLimiterStub) RecordInvalidAuthFailure(key string) {
	s.recorded = append(s.recorded, key)
}

func testInvalidAuthClientKey(t *testing.T, cfg *config.Config, remoteAddr, forwarded, credential string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.Use(SessionBindingContext(cfg))
	var key string
	router.GET("/v1/responses", func(c *gin.Context) {
		key = invalidAuthClientKey(c)
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.RemoteAddr = remoteAddr
	if forwarded != "" {
		req.Header.Set("X-Forwarded-For", forwarded)
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	router.ServeHTTP(httptest.NewRecorder(), req)
	return key
}

func TestInvalidAuthClientKeyAggregatesRotatingCredentialsBySecurityIP(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetForwardedClientIPSettings(false, nil)
	first := testInvalidAuthClientKey(t, cfg, "10.0.0.10:1234", "198.51.100.10", "sk-first")
	second := testInvalidAuthClientKey(t, cfg, "10.0.0.10:1234", "198.51.100.11", "sk-second")
	require.Equal(t, first, second)
	require.Equal(t, "10.0.0.10", first)

	limiter := &invalidAuthLimiterStub{blocked: map[string]bool{first: true}}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Request.RemoteAddr = "10.0.0.10:1234"
	ctx.Request.Header.Set("Authorization", "Bearer sk-second")
	require.True(t, rejectInvalidAuthAbuse(ctx, limiter))
}

func TestInvalidAuthClientKeyMissingCredentialRemainsBounded(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetForwardedClientIPSettings(false, nil)
	first := testInvalidAuthClientKey(t, cfg, "10.0.0.10:1234", "198.51.100.10", "")
	second := testInvalidAuthClientKey(t, cfg, "10.0.0.10:9999", "198.51.100.11", "")
	require.Equal(t, "10.0.0.10", first)
	require.Equal(t, first, second)
}

func TestInvalidAuthClientKeyDoesNotTrustSpoofedForwardedHeader(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetForwardedClientIPSettings(false, []string{"X-Forwarded-For"})
	first := testInvalidAuthClientKey(t, cfg, "203.0.113.7:1234", "198.51.100.10", "sk-same")
	second := testInvalidAuthClientKey(t, cfg, "203.0.113.7:1234", "192.0.2.20", "sk-same")
	require.Equal(t, first, second)
	require.Equal(t, "203.0.113.7", first)
}
