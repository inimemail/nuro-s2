package handler

import (
	"context"
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
	"time"
)

// Buffer no response body: the first successful control-plane response waits
// for old cache-view leases to acknowledge or expire before sending headers.
// Ordinary gateway traffic and sites without existing caches are untouched.
func (h *GatewayHandler) GeminiCachePermissionBarrier(c *gin.Context) {
	if h == nil || h.geminiCachedContentService == nil || !h.geminiCachedContentService.Enabled() || !geminiCachePermissionMutation(c.Request.Method, c.Request.URL.Path) {
		c.Next()
		return
	}
	before, err := h.geminiCachedContentService.AuthorizationRevision(c.Request.Context())
	if err != nil {
		writeCacheManagementError(c, err)
		c.Abort()
		return
	}
	w := &geminiCachePermissionWriter{ResponseWriter: c.Writer, ctx: c.Request.Context(), wait: func(ctx context.Context) error {
		return h.geminiCachedContentService.WaitAuthorizationBarrier(ctx, before, !strings.HasPrefix(c.Request.URL.Path, "/api/v1/admin/accounts/"))
	}}
	c.Writer = w
	c.Next()
}
func geminiCachePermissionMutation(method, path string) bool {
	if method != http.MethodPut && method != http.MethodPatch && method != http.MethodDelete && method != http.MethodPost {
		return false
	}
	for _, prefix := range []string{"/api/v1/admin/accounts/", "/api/v1/admin/groups/", "/api/v1/admin/users/", "/api/v1/admin/api-keys/", "/api/v1/admin/subscriptions/", "/api/v1/keys/"} {
		if strings.HasPrefix(path, prefix) {
			if method != http.MethodPost {
				return true
			}
			for _, action := range []string{"/replace-group", "/batch-update", "/bulk-update", "/batch-delete", "/batch-update-credentials", "/schedulable", "/refresh", "/batch-refresh", "/apply-oauth-credentials", "/models/sync-upstream", "/clear-error", "/revoke", "/restore", "/extend", "/assign", "/bulk-assign"} {
				if strings.HasSuffix(path, action) {
					return true
				}
			}
			return false
		}
	}
	return false
}

type geminiCachePermissionWriter struct {
	gin.ResponseWriter
	ctx             context.Context
	wait            func(context.Context) error
	checked, failed bool
}

func (w *geminiCachePermissionWriter) WriteHeader(code int) {
	if !w.checked && code >= 200 && code < 300 {
		w.checked = true
		ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), 35*time.Second)
		err := w.wait(ctx)
		cancel()
		if err != nil {
			w.failed = true
			w.Header().Del("Content-Length")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.ResponseWriter.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.ResponseWriter.WriteString(`{"error":{"code":503,"message":"Configuration was saved; cache permission propagation is pending. Refresh before retrying.","status":"UNAVAILABLE"}}`)
			return
		}
	}
	if !w.failed {
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *geminiCachePermissionWriter) Write(p []byte) (int, error) {
	if !w.checked {
		w.WriteHeader(w.Status())
	}
	if w.failed {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}
func (w *geminiCachePermissionWriter) WriteString(s string) (int, error) {
	if !w.checked {
		w.WriteHeader(w.Status())
	}
	if w.failed {
		return len(s), nil
	}
	return w.ResponseWriter.WriteString(s)
}

func (w *geminiCachePermissionWriter) WriteHeaderNow() {
	if !w.checked {
		w.WriteHeader(w.Status())
	}
	w.ResponseWriter.WriteHeaderNow()
}
func (w *geminiCachePermissionWriter) Flush() {
	w.WriteHeaderNow()
	w.ResponseWriter.Flush()
}
