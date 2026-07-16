package middleware

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const ContextKeySessionID = "session_id"

func SessionBindingContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		binding := &service.SessionBinding{IP: ip.GetTrustedClientIP(c), UserAgent: c.Request.UserAgent()}
		c.Request = c.Request.WithContext(service.WithSessionBinding(c.Request.Context(), binding))
		c.Next()
	}
}

func enforceSessionBinding(c *gin.Context, authService *service.AuthService, settingService *service.SettingService, auditService *service.AuditLogService, claims *service.JWTClaims) bool {
	if settingService == nil || !settingService.IsSessionBindingEnabled(c.Request.Context()) || claims == nil || claims.BindingHash == "" {
		return true
	}
	current := (&service.SessionBinding{IP: ip.GetTrustedClientIP(c), UserAgent: c.Request.UserAgent()}).Hash()
	if current == "" || current == claims.BindingHash {
		return true
	}
	if authService != nil {
		_ = authService.RevokeSessionFamily(c.Request.Context(), claims.SessionID)
	}
	if auditService != nil {
		uid := claims.UserID
		auditService.Record(&service.AuditLog{ActorUserID: &uid, ActorEmail: claims.Email, ActorRole: claims.Role, AuthMethod: service.AuditAuthMethodJWT, Action: service.AuditActionSessionBindingMismatch, Method: c.Request.Method, Path: c.Request.URL.Path, ClientIP: ip.GetTrustedClientIP(c), UserAgent: c.Request.UserAgent(), StatusCode: 401})
	}
	AbortWithError(c, 401, "SESSION_BINDING_MISMATCH", "Session network fingerprint changed, please login again")
	return false
}
