package middleware

import (
	"bytes"
	"io"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type AuditLogMiddleware gin.HandlerFunc

const (
	ContextKeyAuthEmail = "auth_email"
	auditContextSkip    = "audit_skip"
)

var auditSensitiveReads = map[string]string{
	"GET /api/v1/admin/accounts/data":            "admin.accounts.export",
	"GET /api/v1/admin/proxies/data":             "admin.proxies.export",
	"GET /api/v1/admin/redeem-codes/export":      "admin.redeem_codes.export",
	"GET /api/v1/admin/backups/:id/download-url": "admin.backups.download",
	"GET /api/v1/admin/users/:id/api-keys":       "admin.users.api_keys.read",
	"GET /api/v1/admin/groups/:id/api-keys":      "admin.groups.api_keys.read",
}

var auditBodyOmittedRoutes = map[string]struct{}{
	"POST /api/v1/admin/accounts/import/codex-session": {},
	"POST /api/v1/admin/accounts/data":                 {},
}

func SkipAudit(c *gin.Context) { c.Set(auditContextSkip, true) }

func NewAuditLogMiddleware(auditService *service.AuditLogService) AuditLogMiddleware {
	return AuditLogMiddleware(func(c *gin.Context) {
		routeKey := c.Request.Method + " " + c.FullPath()
		action := auditSensitiveReads[routeKey]
		mutating := c.Request.Method == "POST" || c.Request.Method == "PUT" || c.Request.Method == "PATCH" || c.Request.Method == "DELETE"
		if !mutating && action == "" {
			c.Next()
			return
		}

		body := ""
		if _, omitted := auditBodyOmittedRoutes[routeKey]; omitted {
			body = "<credential-bearing body omitted>"
		} else if c.Request.Body != nil && mutating {
			original := c.Request.Body
			raw, err := io.ReadAll(io.LimitReader(original, service.AuditRequestBodyCaptureLimit+1))
			if err == nil {
				c.Request.Body = &auditRestoredBody{Reader: io.MultiReader(bytes.NewReader(raw), original), closer: original}
				body = service.RedactAuditBody(raw, c.GetHeader("Content-Type"))
			}
		}

		start := time.Now()
		c.Next()
		if c.GetBool(auditContextSkip) {
			return
		}

		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}
		if action == "" {
			action = deriveAuditAction(c.Request.Method, path)
		}
		entry := &service.AuditLog{
			CreatedAt: time.Now().UTC(), Action: action, Method: c.Request.Method, Path: path,
			ClientIP: ip.GetTrustedClientIP(c), UserAgent: c.Request.UserAgent(), RequestBody: body,
			StatusCode: c.Writer.Status(), LatencyMs: time.Since(start).Milliseconds(),
			ActorEmail: c.GetString(ContextKeyAuthEmail), AuthMethod: c.GetString("auth_method"),
			CredentialMasked: MaskedRequestCredential(c),
		}
		if subject, ok := GetAuthSubjectFromContext(c); ok && subject.UserID > 0 {
			uid := subject.UserID
			entry.ActorUserID = &uid
		}
		if role, ok := GetUserRoleFromContext(c); ok {
			entry.ActorRole = role
		}
		if requestID, ok := c.Request.Context().Value(ctxkey.RequestID).(string); ok {
			entry.RequestID = requestID
		}
		extra := map[string]any{}
		if len(c.Params) > 0 {
			params := map[string]string{}
			for _, p := range c.Params {
				params[p.Key] = p.Value
			}
			extra["params"] = params
		}
		if query := service.RedactAuditQuery(c.Request.URL.RawQuery); query != "" {
			extra["query"] = query
		}
		if len(extra) > 0 {
			entry.Extra = extra
		}
		auditService.Record(entry)
	})
}

type auditRestoredBody struct {
	io.Reader
	closer io.Closer
}

func (b *auditRestoredBody) Close() error { return b.closer.Close() }

func MaskedRequestCredential(c *gin.Context) string {
	if key := strings.TrimSpace(c.GetHeader("x-api-key")); key != "" {
		return "x-api-key " + service.MaskAuditCredential(key)
	}
	auth := strings.TrimSpace(c.GetHeader("Authorization"))
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) == 2 {
		return parts[0] + " " + service.MaskAuditCredential(parts[1])
	}
	return service.MaskAuditCredential(auth)
}

func deriveAuditAction(method, path string) string {
	path = strings.Trim(strings.TrimPrefix(path, "/api/v1/"), "/")
	parts := []string{}
	for _, part := range strings.Split(path, "/") {
		if part != "" && !strings.HasPrefix(part, ":") {
			parts = append(parts, strings.ReplaceAll(part, "-", "_"))
		}
	}
	verb := map[string]string{"POST": "create", "PUT": "update", "PATCH": "update", "DELETE": "delete", "GET": "read"}[method]
	if verb == "" {
		verb = strings.ToLower(method)
	}
	return strings.Join(parts, ".") + "." + verb
}
