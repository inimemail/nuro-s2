package middleware

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const ContextKeySessionID = "session_id"

func SessionBindingContext(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		trustForwarded := cfg != nil && cfg.TrustForwardedIPForAPIKeyACL()
		binding := &service.SessionBinding{IP: ip.GetSecurityClientIP(c, trustForwarded), UserAgent: c.Request.UserAgent()}
		c.Request = c.Request.WithContext(service.WithSessionBinding(c.Request.Context(), binding))
		c.Next()
	}
}

func requestSessionBinding(c *gin.Context) *service.SessionBinding {
	if c != nil && c.Request != nil {
		if binding := service.SessionBindingFromContext(c.Request.Context()); binding != nil {
			return binding
		}
	}
	if c == nil || c.Request == nil {
		return &service.SessionBinding{}
	}
	return &service.SessionBinding{IP: ip.GetTrustedClientIP(c), UserAgent: c.Request.UserAgent()}
}

// SecurityClientIP reuses the binding captured before authentication, keeping
// audit records, token issuance and binding validation on the same IP source.
func SecurityClientIP(c *gin.Context) string {
	if binding := requestSessionBinding(c); strings.TrimSpace(binding.IP) != "" {
		return binding.IP
	}
	return ip.GetTrustedClientIP(c)
}

func enforceSessionBinding(c *gin.Context, authService *service.AuthService, settingService *service.SettingService, auditService *service.AuditLogService, claims *service.JWTClaims) bool {
	if settingService == nil || !settingService.IsSessionBindingEnabled(c.Request.Context()) || claims == nil || claims.BindingHash == "" {
		return true
	}
	binding := requestSessionBinding(c)
	current := binding.Hash()
	if current == "" || current == claims.BindingHash {
		return true
	}
	if authService != nil {
		_ = authService.RevokeSessionFamily(c.Request.Context(), claims.SessionID)
	}
	if auditService != nil {
		uid := claims.UserID
		auditService.Record(&service.AuditLog{ActorUserID: &uid, ActorEmail: claims.Email, ActorRole: claims.Role, AuthMethod: service.AuditAuthMethodJWT, Action: service.AuditActionSessionBindingMismatch, Method: c.Request.Method, Path: c.Request.URL.Path, ClientIP: binding.IP, UserAgent: c.Request.UserAgent(), StatusCode: 401})
	}
	AbortWithError(c, 401, "SESSION_BINDING_MISMATCH", "Session network fingerprint changed, please login again")
	return false
}
