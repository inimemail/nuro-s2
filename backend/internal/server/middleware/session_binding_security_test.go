package middleware

import (
	"net/http"
	"net/http/httptest"
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
