package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIEdgeIngressEligiblePath(t *testing.T) {
	require.True(t, openAIEdgeIngressEligiblePath("/v1/chat/completions"))
	require.True(t, openAIEdgeIngressEligiblePath("/openai/v1/chat/completions"))
	require.True(t, openAIEdgeIngressEligiblePath("/v1/responses"))
	require.True(t, openAIEdgeIngressEligiblePath("/openai/v1/responses"))
	require.False(t, openAIEdgeIngressEligiblePath("/v1/responses/compact"))
	require.False(t, openAIEdgeIngressEligiblePath("/v1/images/generations"))
}

func TestOpenAIEdgeIngressURL(t *testing.T) {
	require.Equal(t,
		"http://127.0.0.1:18080/v1/chat/completions?stream=true",
		openAIEdgeIngressURL("127.0.0.1:18080", "/v1/chat/completions?stream=true"),
	)
	require.Equal(t,
		"http://127.0.0.1:18080/v1/responses",
		openAIEdgeIngressURL("http://127.0.0.1:18080/", "/v1/responses"),
	)
}

func TestOpenAIEdgeHopHeadersFiltered(t *testing.T) {
	for _, name := range []string{
		"Content-Length",
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		require.True(t, isOpenAIEdgeHopHeader(name), name)
	}
	require.False(t, isOpenAIEdgeHopHeader("Content-Type"))
	require.False(t, isOpenAIEdgeHopHeader("X-Request-Id"))
}

func TestOpenAIEdgeIngressFallbackHeaderSkipsProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{
		Enabled:             true,
		InternalAPIEnabled:  true,
		InternalSecret:      "secret",
		Mode:                "relay",
		IngressProxyEnabled: true,
		ListenAddr:          "127.0.0.1:1",
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set(openAIEdgeFallbackHeader, "1")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	require.False(t, h.tryOpenAIEdgeIngressProxy(c))
	require.Empty(t, c.Request.Header.Get(openAIEdgeFallbackHeader))
}
