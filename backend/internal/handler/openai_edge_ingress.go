package handler

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/runtimeops"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
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
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || !openAIEdgeSupportsRequestPlatform(c.Request.Context(), apiKey) {
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
	setOpsRequestContext(c, gjson.GetBytes(body, "model").String(), true)

	target := openAIEdgeIngressURL(cfg.ListenAddr, c.Request.URL.RequestURI())
	if target == "" {
		return false
	}
	edgeCtx, cancelEdge := context.WithCancel(c.Request.Context())
	defer cancelEdge()
	req, err := http.NewRequestWithContext(edgeCtx, c.Request.Method, target, bytes.NewReader(body))
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
	// Gateway POSTs are not made idempotent by a forwarded client header.
	// Disable net/http's implicit replay of a reusable body as well.
	req.GetBody = nil
	req.Host = c.Request.Host
	addForwardedHeaders(req.Header, c)
	session, available := reserveEdgeStreamSession(req, body, cfg.ListenAddr, cfg.InternalSecret)
	if !available {
		return false
	}
	if session != nil {
		defer session.close()
	}

	// GotConn is deliberately conservative: once transport owns a connection,
	// an EOF/reset cannot prove that the POST was not accepted by Edge.
	var connected atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	}))
	var resp *http.Response
	if session != nil {
		resp, err = session.start(req)
		if err == nil && resp.StatusCode == http.StatusGone && resp.Header.Get(edgeStreamSessionHeader) == "absent" {
			_ = resp.Body.Close()
			return false // Edge rejected this first send before any execution.
		}
	} else {
		resp, err = doEdgeSessionRequest(req)
	}
	if err != nil && session != nil && c.Request.Context().Err() == nil {
		resp, err = session.resume(0)
	}
	if err != nil {
		if session == nil && !connected.Load() && c.Request.Context().Err() == nil {
			// Only a definite connection establishment failure may use Go.
			var dialErr *net.OpError
			if errors.As(err, &dialErr) && dialErr.Op == "dial" {
				restoreBody()
				return false
			}
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"type": "upstream_error", "message": "Upstream request failed",
		}})
		return true
	}
	if session != nil && resp.StatusCode < 400 {
		resp.Body = &edgeResumingBody{ReadCloser: resp.Body, session: session,
			terminal: openAIEdgeTerminalScanner{responses: strings.HasSuffix(strings.TrimSuffix(c.Request.URL.Path, "/"), "/responses")}}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		if resp.Header.Get("X-Sub2API-Edge-Ops-Owned") == "1" {
			c.Set(service.OpsSkipPassthroughKey, true)
		}
		// Error classification must not wait indefinitely for an incomplete body.
		timer := time.AfterFunc(time.Second, cancelEdge)
		errType, message := openAIEdgeIngressClientError(resp.StatusCode, resp.Body)
		timer.Stop()
		c.JSON(resp.StatusCode, gin.H{
			"error": gin.H{
				"type":    errType,
				"message": message,
			},
		})
		return true
	}

	copyOpenAIEdgeResponseHeaders(c.Writer.Header(), resp.Header)
	// Only the trusted local Edge response can designate callback ownership.
	// Old binaries omit this header and retain ingress-side observation.
	c.Set("edge_ops_callback_owned", resp.Header.Get("X-Sub2API-Edge-Ops-Owned") == "1")
	c.Writer.Header().Del(edgeStreamSessionHeader)
	c.Writer.Header().Del(edgeStreamOffsetHeader)
	c.Writer.Header().Del(edgeStreamBindingHeader)
	c.Status(resp.StatusCode)
	// Commit the SSE headers immediately. This removes the extra Go-hop header
	// delay for systemd deployments where public traffic enters on port 8080.
	c.Writer.Flush()
	copyOpenAIEdgeResponseBody(c, resp.Body, strings.HasSuffix(strings.TrimSuffix(c.Request.URL.Path, "/"), "/responses"))
	return true
}

// The Edge control plane selects and compiles OpenAI accounts only. Composite
// groups must also stay on Go: their resolved platform/model is request context
// and is not carried through the Edge authentication/prepare boundary.
func openAIEdgeSupportsRequestPlatform(ctx context.Context, apiKey *service.APIKey) bool {
	if apiKey == nil {
		return false
	}
	if apiKey.Group != nil && apiKey.Group.Platform != service.PlatformOpenAI {
		return false
	}
	return openAICompatibleRequestPlatform(ctx, apiKey) == service.PlatformOpenAI
}

func openAIEdgeIngressClientError(status int, body io.Reader) (string, string) {
	// Preserve only known public routing errors from the Go fallback. Rebuild
	// the envelope so upstream/CDN details, extra fields and headers cannot leak.
	const limit = 8 << 10
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err == nil && len(data) <= limit && gjson.ValidBytes(data) {
		errType := gjson.GetBytes(data, "error.type").String()
		message := gjson.GetBytes(data, "error.message").String()
		switch {
		case status == http.StatusServiceUnavailable && errType == "api_error" && message == "Service temporarily unavailable":
			return errType, message
		case status == http.StatusServiceUnavailable && errType == "compact_not_supported" &&
			(message == "No available accounts support /responses/compact" || message == "No available accounts support native remote compaction v2"):
			return errType, message
		case status == http.StatusBadRequest && errType == "invalid_request_error" && message == service.OpenAIPoolModelRoutingClientMessage():
			return errType, message
		}
	}
	return "upstream_error", "Upstream request failed"
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
		edgeStreamSessionHeader,
		edgeStreamBindingHeader,
		edgeStreamOffsetHeader,
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
	terminal := openAIEdgeTerminalScanner{responses: responsesDialect}
	localFailure := false
	defer func() {
		callbackOwned, _ := c.Get("edge_ops_callback_owned")
		if terminal.failed && (callbackOwned != true || localFailure) {
			errType := edgeOpsErrorType(terminal.errorType)
			c.Set(edgeOpsStreamFailureKey, true)
			service.MarkOpsStreamError(c, edgeOpsFailureStatus(errType), errType, "Edge stream failed")
		}
	}()
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			terminal.feed(chunk)
			if _, writeErr := dst.Write(chunk); writeErr != nil {
				return
			}
			dst.Flush()
		}
		if readErr != nil {
			if !terminal.seen {
				// A downstream cancellation is not an upstream failure.
				if c.Request == nil || c.Request.Context().Err() == nil {
					terminal.failed = true
					localFailure = true
				}
				// Recovery can exhaust in the middle of an SSE frame. Separate
				// the failure event from its partial data instead of concatenating
				// event:/data: into malformed tool or JSON payload bytes.
				_, _ = dst.Write([]byte("\n\n"))
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
