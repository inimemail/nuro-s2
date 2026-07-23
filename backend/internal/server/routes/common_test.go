package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRuntimeMetricsAccessIsPrivate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			OpenAIEdgeRS: config.GatewayOpenAIEdgeRSConfig{
				InternalSecret: "metrics-secret",
			},
		},
	}
	router := gin.New()
	RegisterCommonRoutes(router, cfg)

	tests := []struct {
		name       string
		remoteAddr string
		secret     string
		forwarded  string
		wantStatus int
	}{
		{name: "IPv4 loopback", remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusOK},
		{name: "IPv6 loopback", remoteAddr: "[::1]:1234", wantStatus: http.StatusOK},
		{name: "loopback reverse proxy hidden", remoteAddr: "127.0.0.1:1234", forwarded: "203.0.113.10", wantStatus: http.StatusNotFound},
		{name: "loopback reverse proxy authenticated", remoteAddr: "127.0.0.1:1234", forwarded: "203.0.113.10", secret: "metrics-secret", wantStatus: http.StatusOK},
		{name: "IPv4 private service monitor", remoteAddr: "10.42.1.8:1234", wantStatus: http.StatusOK},
		{name: "IPv6 private service monitor", remoteAddr: "[fd00::8]:1234", wantStatus: http.StatusOK},
		{name: "private reverse proxy hidden", remoteAddr: "10.42.1.8:1234", forwarded: "203.0.113.10", wantStatus: http.StatusNotFound},
		{name: "private reverse proxy authenticated", remoteAddr: "10.42.1.8:1234", forwarded: "203.0.113.10", secret: "metrics-secret", wantStatus: http.StatusOK},
		{name: "remote hidden", remoteAddr: "203.0.113.10:1234", wantStatus: http.StatusNotFound},
		{name: "remote internal secret", remoteAddr: "203.0.113.10:1234", secret: "metrics-secret", wantStatus: http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			req.RemoteAddr = test.remoteAddr
			if test.secret != "" {
				req.Header.Set("X-Sub2API-Edge-Secret", test.secret)
			}
			if test.forwarded != "" {
				req.Header.Set("X-Forwarded-For", test.forwarded)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, test.wantStatus, response.Code)
			if test.wantStatus == http.StatusOK {
				require.Contains(t, response.Body.String(), "sub2api_go_process_uptime_seconds")
			}
		})
	}
}
