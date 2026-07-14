package handler

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIEdgeFallbackHeader = "X-Sub2API-Edge-Fallback"
const openAIEdgeFallbackReasonHeader = "X-Sub2API-Edge-Fallback-Reason"

var openAIEdgeIngressClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	},
}

func (h *OpenAIGatewayHandler) tryOpenAIEdgeIngressProxy(c *gin.Context) bool {
	if h == nil || c == nil || c.Request == nil {
		return false
	}
	cfg := h.openAIEdgeConfig()
	if !cfg.Enabled || !cfg.InternalAPIEnabled || !cfg.IngressProxyEnabled {
		return false
	}
	if strings.ToLower(strings.TrimSpace(cfg.Mode)) != "relay" {
		return false
	}
	if strings.TrimSpace(cfg.InternalSecret) == "" || strings.TrimSpace(cfg.ListenAddr) == "" {
		return false
	}
	if strings.TrimSpace(c.GetHeader(openAIEdgeFallbackHeader)) != "" {
		applyOpenAIEdgeFallbackContext(c)
		clearOpenAIEdgeFallbackHeaders(c.Request.Header)
		return false
	}
	if c.Request.Method != http.MethodPost {
		return false
	}
	if c.Request.Body == nil {
		return false
	}
	if !openAIEdgeIngressEligiblePath(c.Request.URL.Path) {
		return false
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.Request.Body = io.NopCloser(bytes.NewReader(nil))
		c.Request.ContentLength = 0
		return false
	}
	restoreBody := func() {
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
	}
	restoreBody()
	if !gjson.GetBytes(body, "stream").Bool() {
		return false
	}

	target := openAIEdgeIngressURL(cfg.ListenAddr, c.Request.URL.RequestURI())
	if target == "" {
		return false
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, target, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header = c.Request.Header.Clone()
	clearOpenAIEdgeFallbackHeaders(req.Header)
	for name := range req.Header {
		if isOpenAIEdgeHopHeader(name) {
			req.Header.Del(name)
		}
	}
	req.ContentLength = int64(len(body))
	req.Host = c.Request.Host
	addForwardedHeaders(req.Header, c)

	resp, err := openAIEdgeIngressClient.Do(req)
	if err != nil {
		restoreBody()
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		// The edge relay is an internal hop. Never copy an upstream/CDN error
		// page or its headers to the public client.
		c.JSON(resp.StatusCode, gin.H{
			"error": gin.H{
				"type":    "upstream_error",
				"message": "Upstream request failed",
			},
		})
		return true
	}

	copyOpenAIEdgeResponseHeaders(c.Writer.Header(), resp.Header)
	c.Status(resp.StatusCode)
	copyOpenAIEdgeResponseBody(c.Writer, resp.Body)
	return true
}

func clearOpenAIEdgeFallbackHeaders(header http.Header) {
	if header == nil {
		return
	}
	for _, name := range []string{
		openAIEdgeFallbackHeader,
		openAIEdgeFallbackReasonHeader,
		"X-Sub2API-Edge-Prepare-Ms",
		"X-Sub2API-Edge-Queue-Wait-Ms",
		"X-Sub2API-Edge-Relay-Start-Ms",
		"X-Sub2API-Edge-Retry-Count",
	} {
		header.Del(name)
	}
}

func applyOpenAIEdgeFallbackContext(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	ctx := c.Request.Context()
	if reason := strings.TrimSpace(c.GetHeader(openAIEdgeFallbackReasonHeader)); reason != "" {
		ctx = context.WithValue(ctx, ctxkey.EdgeFallbackReason, reason)
	}
	for _, item := range []struct {
		header string
		key    ctxkey.Key
	}{
		{"X-Sub2API-Edge-Prepare-Ms", ctxkey.EdgePrepareMs},
		{"X-Sub2API-Edge-Queue-Wait-Ms", ctxkey.EdgeQueueWaitMs},
		{"X-Sub2API-Edge-Relay-Start-Ms", ctxkey.EdgeRelayStartMs},
		{"X-Sub2API-Edge-Retry-Count", ctxkey.EdgeRetryCount},
	} {
		value := strings.TrimSpace(c.GetHeader(item.header))
		if value == "" {
			continue
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			continue
		}
		ctx = context.WithValue(ctx, item.key, parsed)
	}
	c.Request = c.Request.WithContext(ctx)
}

func openAIEdgeIngressEligiblePath(path string) bool {
	switch {
	case strings.HasSuffix(path, "/v1/chat/completions"):
		return true
	case strings.HasSuffix(path, "/v1/responses"):
		return true
	default:
		return false
	}
}

func openAIEdgeIngressURL(listenAddr, requestURI string) string {
	base := strings.TrimSpace(listenAddr)
	if base == "" {
		return ""
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	base = strings.TrimRight(base, "/")
	if requestURI == "" || requestURI[0] != '/' {
		requestURI = "/" + requestURI
	}
	return base + requestURI
}

func addForwardedHeaders(header http.Header, c *gin.Context) {
	if header == nil || c == nil || c.Request == nil {
		return
	}
	remoteIP := c.ClientIP()
	if remoteIP == "" {
		if host, _, err := net.SplitHostPort(c.Request.RemoteAddr); err == nil {
			remoteIP = host
		}
	}
	if remoteIP != "" {
		if prior := strings.TrimSpace(header.Get("X-Forwarded-For")); prior != "" {
			header.Set("X-Forwarded-For", prior+", "+remoteIP)
		} else {
			header.Set("X-Forwarded-For", remoteIP)
		}
		header.Set("X-Real-IP", remoteIP)
	}
	if c.Request.TLS != nil {
		header.Set("X-Forwarded-Proto", "https")
	} else if header.Get("X-Forwarded-Proto") == "" {
		header.Set("X-Forwarded-Proto", "http")
	}
	if c.Request.Host != "" {
		header.Set("X-Forwarded-Host", c.Request.Host)
	}
}

func copyOpenAIEdgeResponseHeaders(dst, src http.Header) {
	responseheaders.WriteFilteredHeaders(dst, src, nil)
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "text/event-stream")
	}
}

func copyOpenAIEdgeResponseBody(dst gin.ResponseWriter, src io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
			dst.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

func isOpenAIEdgeHopHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "content-length", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
