package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *UserHandler) blacklistActor(c *gin.Context) (int64, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	role, _ := middleware.GetUserRoleFromContext(c)
	method, _ := c.Get("auth_method")
	if !ok || role != service.RoleAdmin || method != service.AuditAuthMethodJWT {
		response.Forbidden(c, "Administrator session required")
		return 0, false
	}
	if h.userService == nil {
		response.InternalError(c, "User service unavailable")
		return 0, false
	}
	return subject.UserID, true
}
func (h *UserHandler) PreviewRegistrationBlacklist(c *gin.Context) {
	actor, ok := h.blacklistActor(c)
	if !ok {
		return
	}
	after, err := strconv.ParseInt(c.DefaultQuery("after_id", "0"), 10, 64)
	if err != nil || after < 0 {
		response.BadRequest(c, "Invalid preview position")
		return
	}
	result, err := h.userService.PreviewRegistrationBlacklist(c.Request.Context(), actor, after)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
func (h *UserHandler) DisableRegistrationBlacklist(c *gin.Context) {
	actor, ok := h.blacklistActor(c)
	if !ok {
		return
	}
	var req service.RegistrationBlacklistDisableRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid confirmed batch")
		return
	}
	ids, err := h.userService.DisableRegistrationBlacklist(c.Request.Context(), actor, req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"disabled_ids": ids})
}
