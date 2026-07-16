package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type AuditLogHandler struct {
	auditService *service.AuditLogService
	totpService  *service.TotpService
}

func NewAuditLogHandler(auditService *service.AuditLogService, totpService *service.TotpService) *AuditLogHandler {
	return &AuditLogHandler{auditService: auditService, totpService: totpService}
}

func (h *AuditLogHandler) List(c *gin.Context) {
	page, size := response.ParsePagination(c)
	if size > 200 {
		size = 200
	}
	filter := &service.AuditLogFilter{Page: page, PageSize: size, ActorEmail: strings.TrimSpace(c.Query("actor_email")), AuthMethod: strings.TrimSpace(c.Query("auth_method")), Action: strings.TrimSpace(c.Query("action")), Method: strings.TrimSpace(c.Query("method")), ClientIP: strings.TrimSpace(c.Query("client_ip")), Query: strings.TrimSpace(c.Query("q"))}
	if raw := strings.TrimSpace(c.Query("actor_user_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid actor_user_id")
			return
		}
		filter.ActorUserID = &id
	}
	if raw := strings.TrimSpace(c.Query("start_time")); raw != "" {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			response.BadRequest(c, "Invalid start_time")
			return
		}
		filter.StartTime = &value
	}
	if raw := strings.TrimSpace(c.Query("end_time")); raw != "" {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			response.BadRequest(c, "Invalid end_time")
			return
		}
		filter.EndTime = &value
	}
	if raw := strings.TrimSpace(c.Query("success")); raw != "" {
		value := raw == "true"
		filter.Success = &value
	}
	result, err := h.auditService.List(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, result.Logs, int64(result.Total), result.Page, result.PageSize)
}

func (h *AuditLogHandler) Get(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid audit log id")
		return
	}
	item, err := h.auditService.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *AuditLogHandler) Clear(c *gin.Context) {
	if c.GetString("auth_method") == service.AuditAuthMethodAdminAPIKey {
		response.Forbidden(c, "A two-factor verified admin session is required")
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "Unauthorized")
		return
	}
	var req struct {
		TotpCode string `json:"totp_code" binding:"required"`
	}
	if c.ShouldBindJSON(&req) != nil {
		response.BadRequest(c, "TOTP code is required")
		return
	}
	if err := h.totpService.VerifyCode(c.Request.Context(), subject.UserID, strings.TrimSpace(req.TotpCode)); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	uid := subject.UserID
	role, _ := middleware.GetUserRoleFromContext(c)
	requestID, _ := c.Request.Context().Value(ctxkey.RequestID).(string)
	trace := &service.AuditLog{ActorUserID: &uid, ActorEmail: c.GetString(middleware.ContextKeyAuthEmail), ActorRole: role, AuthMethod: c.GetString("auth_method"), CredentialMasked: middleware.MaskedRequestCredential(c), Method: http.MethodPost, Path: c.FullPath(), RequestID: strings.TrimSpace(requestID), ClientIP: ip.GetTrustedClientIP(c), UserAgent: c.Request.UserAgent(), StatusCode: http.StatusOK}
	deleted, err := h.auditService.ClearAll(c.Request.Context(), trace)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	middleware.SkipAudit(c)
	response.Success(c, gin.H{"deleted": deleted})
}
