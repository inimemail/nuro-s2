package handler

import (
	"bytes"
	"context"
	"crypto/subtle"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/runtimeops"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIEdgeFallbackHeader = "X-Sub2API-Edge-Fallback"
const openAIEdgeFallbackReasonHeader = "X-Sub2API-Edge-Fallback-Reason"
const openAIEdgeContinuationHeader = "X-Sub2API-Edge-Continuation"

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
	if strings.TrimSpace(c.GetHeader(openAIEdgeFallbackHeader)) != "" {
		secret := strings.TrimSpace(c.GetHeader(openAIEdgeSecretHeader))
		if cfg.InternalAPIEnabled && strings.TrimSpace(cfg.InternalSecret) != "" &&
			subtle.ConstantTimeCompare([]byte(secret), []byte(strings.TrimSpace(cfg.InternalSecret))) == 1 {
			applyOpenAIEdgeFallbackContext(h, c)
		}
		clearOpenAIEdgeFallbackHeaders(c.Request.Header)
		return false
	}
	if !cfg.Enabled || !cfg.InternalAPIEnabled || !cfg.IngressProxyEnabled {
		return false
	}
	if strings.ToLower(strings.TrimSpace(cfg.Mode)) != "relay" {
		return false
	}
	if strings.TrimSpace(cfg.InternalSecret) == "" || strings.TrimSpace(cfg.ListenAddr) == "" {
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
	// Commit the SSE headers immediately. This removes the extra Go-hop header
	// delay for systemd deployments where public traffic enters on port 8080.
	c.Writer.Flush()
	copyOpenAIEdgeResponseBody(c, resp.Body, strings.HasSuffix(strings.TrimSuffix(c.Request.URL.Path, "/"), "/responses"))
	return true
}

func clearOpenAIEdgeFallbackHeaders(header http.Header) {
	if header == nil {
		return
	}
	for _, name := range []string{
		openAIEdgeFallbackHeader,
		openAIEdgeFallbackReasonHeader,
		openAIEdgeContinuationHeader,
		"X-Sub2API-Edge-Prepare-Ms",
		"X-Sub2API-Edge-Queue-Wait-Ms",
		"X-Sub2API-Edge-Relay-Start-Ms",
		"X-Sub2API-Edge-Retry-Count",
		openAIEdgeSecretHeader,
	} {
		header.Del(name)
	}
}

func applyOpenAIEdgeFallbackContext(h *OpenAIGatewayHandler, c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	ctx := c.Request.Context()
	continuationRestored := false
	if reason := strings.TrimSpace(c.GetHeader(openAIEdgeFallbackReasonHeader)); reason != "" {
		ctx = context.WithValue(ctx, ctxkey.EdgeFallbackReason, reason)
	}
	if token := strings.TrimSpace(c.GetHeader(openAIEdgeContinuationHeader)); token != "" {
		if state, ok := h.consumeOpenAIEdgeContinuation(ctx, token); ok {
			ctx = context.WithValue(ctx, ctxkey.EdgeRetryContinuation, state)
			continuationRestored = true
		}
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
		if item.key == ctxkey.EdgeRetryCount && parsed > 0 && !continuationRestored {
			runtimeops.ObserveEdgeContinuationMissing()
		}
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
	mediaType, _, _ := mime.ParseMediaType(src.Get("Content-Type"))
	if strings.EqualFold(mediaType, "text/event-stream") {
		dst.Set("Content-Type", "text/event-stream")
		dst.Set("Cache-Control", "no-cache, no-transform")
		dst.Set("X-Accel-Buffering", "no")
	}
}

func copyOpenAIEdgeResponseBody(c *gin.Context, src io.Reader, responsesDialect bool) {
	if c == nil {
		return
	}
	dst := c.Writer
	buf := make([]byte, 32*1024)
	const terminalScanTail = 128
	var tail []byte
	terminalSeen := false
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			scan := make([]byte, 0, len(tail)+len(chunk))
			scan = append(scan, tail...)
			scan = append(scan, chunk...)
			if bytes.Contains(scan, []byte(`"type":"response.completed"`)) ||
				bytes.Contains(scan, []byte(`"type":"response.failed"`)) ||
				bytes.Contains(scan, []byte(`"type":"response.incomplete"`)) ||
				bytes.Contains(scan, []byte(`"type":"response.cancelled"`)) ||
				bytes.Contains(scan, []byte(`"type":"response.canceled"`)) ||
				bytes.Contains(scan, []byte(`"type":"response.done"`)) ||
				bytes.Contains(scan, []byte("event: response.completed")) ||
				bytes.Contains(scan, []byte("event: response.failed")) ||
				bytes.Contains(scan, []byte("event: response.incomplete")) ||
				bytes.Contains(scan, []byte("event: response.cancelled")) ||
				bytes.Contains(scan, []byte("event: response.canceled")) ||
				bytes.Contains(scan, []byte("event: response.done")) ||
				bytes.Contains(scan, []byte("data: [DONE]")) {
				terminalSeen = true
			}
			if len(scan) > terminalScanTail {
				tail = append(tail[:0], scan[len(scan)-terminalScanTail:]...)
			} else {
				tail = append(tail[:0], scan...)
			}
			if _, writeErr := dst.Write(chunk); writeErr != nil {
				return
			}
			dst.Flush()
		}
		if readErr != nil {
			if !terminalSeen {
				if responsesDialect {
					_ = writeResponsesFailedSSE(c, "server_error", "Upstream request failed")
				} else {
					_, _ = dst.Write([]byte("data: {\"error\":{\"type\":\"upstream_error\",\"message\":\"Upstream request failed\"}}\n\ndata: [DONE]\n\n"))
				}
				dst.Flush()
			}
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
