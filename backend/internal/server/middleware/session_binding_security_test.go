package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSessionBindingContextUsesLiveSecurityIPToggle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.SetTrustForwardedIPForAPIKeyACL(false)

	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.Use(SessionBindingContext(cfg))
	router.GET("/binding", func(c *gin.Context) {
		binding := service.SessionBindingFromContext(c.Request.Context())
		require.NotNil(t, binding)
		c.JSON(http.StatusOK, gin.H{"binding_ip": binding.IP, "security_ip": SecurityClientIP(c)})
	})

	request := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/binding", nil)
		req.RemoteAddr = "9.9.9.9:12345"
		req.Header.Set("CF-Connecting-IP", "1.2.3.4")
		router.ServeHTTP(recorder, req)
		return recorder
	}

	recorder := request()
	require.JSONEq(t, `{"binding_ip":"9.9.9.9","security_ip":"9.9.9.9"}`, recorder.Body.String())

	cfg.SetTrustForwardedIPForAPIKeyACL(true)
	recorder = request()
	require.JSONEq(t, `{"binding_ip":"1.2.3.4","security_ip":"1.2.3.4"}`, recorder.Body.String())
}

func TestSessionBindingContextBoundsPersistedUserAgent(t *testing.T) {
	cfg := &config.Config{}
	router := gin.New()
	router.Use(SessionBindingContext(cfg))
	router.GET("/t", func(c *gin.Context) {
		binding := service.SessionBindingFromContext(c.Request.Context())
		require.NotNil(t, binding)
		require.Len(t, binding.UserAgent, maxPersistentUserAgentBytes)
		require.Len(t, c.Request.UserAgent(), 2048)
		require.NotEqual(t, binding.UserAgent, c.Request.UserAgent())
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.Header.Set("User-Agent", strings.Repeat("u", 2048))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestSessionBindingContextV162SnapshotsCustomClientIPHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.SetForwardedClientIPSettings(true, []string{"True-Client-IP"})

	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.Use(SessionBindingContext(cfg))
	router.GET("/binding", func(c *gin.Context) {
		// A live settings change after request admission must not change the
		// security identity already captured for this request.
		cfg.SetForwardedClientIPSettings(true, []string{"X-Other-IP"})
		binding := service.SessionBindingFromContext(c.Request.Context())
		require.NotNil(t, binding)
		c.JSON(http.StatusOK, gin.H{"binding_ip": binding.IP, "security_ip": SecurityClientIP(c)})
	})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/binding", nil)
	req.RemoteAddr = "9.9.9.9:12345"
	req.Header.Set("True-Client-IP", "203.0.113.10")
	req.Header.Set("CF-Connecting-IP", "198.51.100.20")
	req.Header.Set("X-Other-IP", "192.0.2.30")
	router.ServeHTTP(recorder, req)

	require.JSONEq(t, `{"binding_ip":"203.0.113.10","security_ip":"203.0.113.10"}`, recorder.Body.String())
}
