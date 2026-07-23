package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type AuditLogMiddleware gin.HandlerFunc

const (
	ContextKeyAuthEmail   = "auth_email"
	auditContextAction    = "audit_action"
	auditContextActorID   = "audit_actor_id"
	auditContextActorMail = "audit_actor_email"
	auditContextSkip      = "audit_skip"
)

// SetAuditAction gives operations with non-CRUD semantics a stable action name.
func SetAuditAction(c *gin.Context, action string) {
	if c != nil && strings.TrimSpace(action) != "" {
		c.Set(auditContextAction, strings.TrimSpace(action))
	}
}

// SetAuditActor records an authenticated actor for public auth endpoints.
func SetAuditActor(c *gin.Context, userID int64, email string) {
	if c == nil {
		return
	}
	if userID > 0 {
		c.Set(auditContextActorID, userID)
	}
	if email = strings.TrimSpace(email); email != "" {
		c.Set(auditContextActorMail, email)
	}
}

var auditSensitiveReads = map[string]string{
	"GET /api/v1/admin/accounts/data":               "admin.accounts.export",
	"GET /api/v1/admin/proxies/data":                "admin.proxies.export",
	"GET /api/v1/admin/redeem-codes/export":         "admin.redeem_codes.export",
	"GET /api/v1/admin/backups/:id/download-url":    "admin.backups.download",
	"GET /api/v1/admin/settings/admin-api-key":      "admin.admin_api_key.read",
	"GET /api/v1/admin/users/:id/api-keys":          "admin.users.api_keys.read",
	"GET /api/v1/admin/groups/:id/api-keys":         "admin.groups.api_keys.read",
	"GET /api/v1/admin/backups/s3-config":           "admin.backups.s3_config.read",
	"GET /api/v1/admin/data-management/config":      "admin.data_management.config.read",
	"GET /api/v1/admin/data-management/s3/profiles": "admin.data_management.s3_profiles.read",
	"GET /api/v1/admin/prompt-audit/config":         "admin.prompt_audit.config.read",
	"GET /api/v1/admin/prompt-audit/runtime":        "admin.prompt_audit.runtime.read",
	"GET /api/v1/admin/prompt-audit/events":         "admin.prompt_audit.events.list",
	"GET /api/v1/admin/prompt-audit/events/:id":     "admin.prompt_audit.events.read",
}

var auditActionOverrides = map[string]string{
	"POST /api/v1/auth/login":                              service.AuditActionLogin,
	"POST /api/v1/auth/login/2fa":                          service.AuditActionLogin2FA,
	"POST /api/v1/auth/register":                           service.AuditActionRegister,
	"POST /api/v1/auth/refresh":                            service.AuditActionTokenRefresh,
	"POST /api/v1/user/totp/step-up":                       service.AuditActionStepUpVerify,
	"POST /api/v1/admin/audit-logs/clear":                  service.AuditActionAuditLogClear,
	"POST /api/v1/admin/accounts/data":                     "admin.accounts.import",
	"POST /api/v1/admin/backups":                           "admin.backups.create",
	"POST /api/v1/admin/backups/:id/restore":               "admin.backups.restore",
	"DELETE /api/v1/admin/backups/:id":                     "admin.backups.delete",
	"PUT /api/v1/admin/backups/s3-config":                  "admin.backups.s3_config.update",
	"POST /api/v1/admin/backups/s3-config/test":            "admin.backups.s3_config.test",
	"POST /api/v1/admin/data-management/s3/test":           "admin.data_management.s3.test",
	"POST /api/v1/admin/settings/admin-api-key/regenerate": "admin.admin_api_key.regenerate",
	"DELETE /api/v1/admin/settings/admin-api-key":          "admin.admin_api_key.delete",
}

var auditBodyOmittedRoutes = map[string]struct{}{
	"PUT /api/v1/admin/accounts/:id/ollama-cloud-usage/session": {},
	"POST /api/v1/admin/accounts/import/codex-session":          {},
	"POST /api/v1/admin/accounts/data":                          {},
}

func SkipAudit(c *gin.Context) { c.Set(auditContextSkip, true) }

func NewAuditLogMiddleware(auditService *service.AuditLogService) AuditLogMiddleware {
	return AuditLogMiddleware(func(c *gin.Context) {
		routeKey := c.Request.Method + " " + c.FullPath()
		mutating := c.Request.Method == "POST" || c.Request.Method == "PUT" || c.Request.Method == "PATCH" || c.Request.Method == "DELETE"
		action := auditActionOverrides[routeKey]
		if !mutating {
			action = auditSensitiveReads[routeKey]
		}
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
			// Always prepend the consumed prefix, including when the audit read
			// failed. A transient reader error must not alter what the handler sees.
			c.Request.Body = &auditRestoredBody{Reader: io.MultiReader(bytes.NewReader(raw), original), closer: original}
			if err == nil {
				body = service.RedactAuditBody(raw, c.GetHeader("Content-Type"))
			}
		}

		start := time.Now()
		c.Next()
		if c.GetBool(auditContextSkip) {
			return
		}
		status := c.Writer.Status()
		// Successful refreshes are high-frequency maintenance; failures remain
		// valuable security signals and retain a stable action name.
		if routeKey == "POST /api/v1/auth/refresh" && status < http.StatusBadRequest {
			return
		}

		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}
		if action == "" {
			action = deriveAuditAction(c.Request.Method, path)
		}
		if overridden, ok := c.Get(auditContextAction); ok {
			if value, ok := overridden.(string); ok && strings.TrimSpace(value) != "" {
				action = strings.TrimSpace(value)
			}
		}
		entry := &service.AuditLog{
			CreatedAt: time.Now().UTC(), Action: action, Method: c.Request.Method, Path: path,
			ClientIP: SecurityClientIP(c), UserAgent: c.Request.UserAgent(), RequestBody: body,
			StatusCode: status, LatencyMs: time.Since(start).Milliseconds(),
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
		if entry.AuthMethod == "" && entry.ActorUserID != nil {
			entry.AuthMethod = service.AuditAuthMethodJWT
		}
		if value, ok := c.Get(auditContextActorID); ok {
			if userID, ok := value.(int64); ok && userID > 0 {
				entry.ActorUserID = &userID
			}
		}
		if value, ok := c.Get(auditContextActorMail); ok {
			if email, ok := value.(string); ok && strings.TrimSpace(email) != "" {
				entry.ActorEmail = strings.TrimSpace(email)
			}
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
	if key := strings.TrimSpace(c.GetHeader("x-goog-api-key")); key != "" {
		return "x-goog-api-key " + service.MaskAuditCredential(key)
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
		if part != "" && !strings.HasPrefix(part, ":") && !strings.HasPrefix(part, "*") {
			parts = append(parts, strings.ReplaceAll(part, "-", "_"))
		}
	}
	verb := map[string]string{"POST": "create", "PUT": "update", "PATCH": "update", "DELETE": "delete", "GET": "read"}[method]
	if verb == "" {
		verb = strings.ToLower(method)
	}
	return strings.Join(parts, ".") + "." + verb
}
