package middleware

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type StepUpAuthMiddleware gin.HandlerFunc

type stepUpGrantChecker interface {
	HasStepUpGrant(context.Context, int64, string) (bool, error)
}
type stepUpUserReader interface {
	GetByID(context.Context, int64) (*service.User, error)
}

func StepUpSessionKey(c *gin.Context, userID int64) string {
	if sessionID := c.GetString(ContextKeySessionID); sessionID != "" {
		return sessionID
	}
	return fmt.Sprintf("u%d", userID)
}

func NewStepUpAuthMiddleware(totpService *service.TotpService, userService *service.UserService) StepUpAuthMiddleware {
	return StepUpAuthMiddleware(stepUpAuth(totpService, userService))
}

func stepUpAuth(checker stepUpGrantChecker, users stepUpUserReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		if enforceStepUp(c, checker, users) {
			c.Next()
		}
	}
}

func EnforceStepUp(c *gin.Context, totpService *service.TotpService, userService *service.UserService) bool {
	return enforceStepUp(c, totpService, userService)
}

func enforceStepUp(c *gin.Context, checker stepUpGrantChecker, users stepUpUserReader) bool {
	if c.GetString("auth_method") == service.AuditAuthMethodAdminAPIKey {
		AbortWithError(c, 403, "STEP_UP_ADMIN_API_KEY_FORBIDDEN", "A two-factor verified admin session is required")
		return false
	}
	subject, ok := GetAuthSubjectFromContext(c)
	if !ok || checker == nil || users == nil {
		AbortWithError(c, 401, "UNAUTHORIZED", "Authorization required")
		return false
	}
	user, err := users.GetByID(c.Request.Context(), subject.UserID)
	if err != nil || user == nil {
		AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to load user")
		return false
	}
	if !user.TotpEnabled {
		AbortWithError(c, 403, "STEP_UP_TOTP_NOT_ENABLED", "Enable TOTP before this operation")
		return false
	}
	granted, err := checker.HasStepUpGrant(c.Request.Context(), subject.UserID, StepUpSessionKey(c, subject.UserID))
	if err != nil {
		AbortWithError(c, 503, "STEP_UP_UNAVAILABLE", "Step-up verification service unavailable")
		return false
	}
	if !granted {
		AbortWithError(c, 403, "STEP_UP_REQUIRED", "Recent two-factor verification is required")
		return false
	}
	return true
}
