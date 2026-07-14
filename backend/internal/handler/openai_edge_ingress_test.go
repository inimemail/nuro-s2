package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
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

func TestCopyOpenAIEdgeResponseHeadersSanitizesUpstreamIdentity(t *testing.T) {
	src := http.Header{
		"Content-Type": {"text/event-stream; charset=utf-8; provider=private-upstream.example"},
		"X-Request-Id": {"openai-req-private"},
		"Server":       {"private-upstream.example"},
	}
	dst := make(http.Header)

	copyOpenAIEdgeResponseHeaders(dst, src)

	require.Equal(t, "text/event-stream", dst.Get("Content-Type"))
	require.Regexp(t, `^req_[0-9a-f]{24}$`, dst.Get("X-Request-Id"))
	require.NotEqual(t, src.Get("X-Request-Id"), dst.Get("X-Request-Id"))
	require.Empty(t, dst.Get("Server"))
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

func TestOpenAIEdgeIngressFallbackHeadersBecomeContext(t *testing.T) {
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
	req.Header.Set(openAIEdgeFallbackReasonHeader, "prepare_fallback_go")
	req.Header.Set("X-Sub2API-Edge-Prepare-Ms", "12")
	req.Header.Set("X-Sub2API-Edge-Retry-Count", "2")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	require.False(t, h.tryOpenAIEdgeIngressProxy(c))
	require.Empty(t, c.Request.Header.Get(openAIEdgeFallbackHeader))
	require.Empty(t, c.Request.Header.Get(openAIEdgeFallbackReasonHeader))
	require.Empty(t, c.Request.Header.Get("X-Sub2API-Edge-Prepare-Ms"))
	require.Equal(t, "prepare_fallback_go", c.Request.Context().Value(ctxkey.EdgeFallbackReason))
	require.Equal(t, int64(12), c.Request.Context().Value(ctxkey.EdgePrepareMs))
	require.Equal(t, int64(2), c.Request.Context().Value(ctxkey.EdgeRetryCount))
}
