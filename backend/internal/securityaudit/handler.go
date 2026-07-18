package securityaudit

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

type PromptAdminHandler struct{ service *Service }

func NewPromptAdminHandler(service *Service) *PromptAdminHandler {
	return &PromptAdminHandler{service: service}
}

func (h *PromptAdminHandler) available(c *gin.Context) *Service {
	if h == nil || h.service == nil {
		response.ErrorFrom(c, infraerrors.New(http.StatusServiceUnavailable, "PROMPT_AUDIT_UNAVAILABLE", "prompt audit service is unavailable"))
		return nil
	}
	if !h.service.FeatureEnabledFast() {
		response.ErrorFrom(c, infraerrors.NotFound("PROMPT_AUDIT_DISABLED", "prompt audit is disabled"))
		return nil
	}
	return h.service
}

func (h *PromptAdminHandler) GetConfig(c *gin.Context) {
	if svc := h.available(c); svc == nil {
		return
	} else {
		response.Success(c, svc.PublicConfig())
	}
}

func (h *PromptAdminHandler) UpdateConfig(c *gin.Context) {
	svc := h.available(c)
	if svc == nil {
		return
	}
	var req UpdateConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("PROMPT_AUDIT_REQUEST_INVALID", "invalid prompt audit config request"))
		return
	}
	cfg, err := svc.SaveConfig(c.Request.Context(), req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

func (h *PromptAdminHandler) ProbeEndpoint(c *gin.Context) {
	svc := h.available(c)
	if svc == nil {
		return
	}
	var req struct {
		Endpoint UpdateEndpoint `json:"endpoint"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("PROMPT_AUDIT_PROBE_INVALID", "invalid endpoint probe request"))
		return
	}
	result, err := svc.Probe(c.Request.Context(), req.Endpoint)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *PromptAdminHandler) GetRuntime(c *gin.Context) {
	if svc := h.available(c); svc != nil {
		response.Success(c, svc.Runtime())
	}
}

func (h *PromptAdminHandler) ListEvents(c *gin.Context) {
	svc := h.available(c)
	if svc == nil {
		return
	}
	filter, err := promptEventFilterFromQuery(c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	result, err := svc.ListEvents(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *PromptAdminHandler) GetEvent(c *gin.Context) {
	svc := h.available(c)
	if svc == nil {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.ErrorFrom(c, infraerrors.BadRequest("PROMPT_AUDIT_EVENT_ID_INVALID", "invalid event id"))
		return
	}
	result, err := svc.GetEvent(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		response.ErrorFrom(c, infraerrors.NotFound("PROMPT_AUDIT_EVENT_NOT_FOUND", "prompt audit event not found"))
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *PromptAdminHandler) DeleteEvent(c *gin.Context) {
	svc := h.available(c)
	if svc == nil {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.ErrorFrom(c, infraerrors.BadRequest("PROMPT_AUDIT_EVENT_ID_INVALID", "invalid event id"))
		return
	}
	deleted, err := svc.DeleteEvent(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": deleted})
}

func (h *PromptAdminHandler) BatchDelete(c *gin.Context) {
	svc := h.available(c)
	if svc == nil {
		return
	}
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 || len(req.IDs) > 1000 {
		response.ErrorFrom(c, infraerrors.BadRequest("PROMPT_AUDIT_DELETE_INVALID", "event ids are invalid"))
		return
	}
	deleted, err := svc.DeleteEvents(c.Request.Context(), req.IDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": deleted})
}

func (h *PromptAdminHandler) DeletePreview(c *gin.Context) {
	svc := h.available(c)
	if svc == nil {
		return
	}
	var req struct {
		Filter EventFilter `json:"filter"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("PROMPT_AUDIT_DELETE_FILTER_INVALID", "delete filter is invalid"))
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.ErrorFrom(c, infraerrors.Unauthorized("UNAUTHORIZED", "authorization required"))
		return
	}
	preview, err := svc.CreateDeletePreview(c.Request.Context(), req.Filter, subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, preview)
}

func (h *PromptAdminHandler) DeleteByFilter(c *gin.Context) {
	svc := h.available(c)
	if svc == nil {
		return
	}
	var req struct {
		ConfirmationToken string `json:"confirmation_token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.ConfirmationToken) == "" {
		response.ErrorFrom(c, infraerrors.BadRequest("PROMPT_AUDIT_DELETE_CONFIRMATION_REQUIRED", "delete confirmation token is required"))
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.ErrorFrom(c, infraerrors.Unauthorized("UNAUTHORIZED", "authorization required"))
		return
	}
	deleted, err := svc.DeleteByFilter(c.Request.Context(), req.ConfirmationToken, subject.UserID)
	if err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("PROMPT_AUDIT_DELETE_CONFIRMATION_INVALID", "delete confirmation token is invalid"))
		return
	}
	response.Success(c, gin.H{"deleted": deleted})
}

func promptEventFilterFromQuery(c *gin.Context) (EventFilter, error) {
	filter := EventFilter{Page: 1, PageSize: 20, Decision: strings.TrimSpace(c.Query("decision")), RiskLevel: strings.TrimSpace(c.Query("risk_level")), Search: strings.TrimSpace(c.Query("search"))}
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return EventFilter{}, infraerrors.BadRequest("PROMPT_AUDIT_PAGE_INVALID", "page is invalid")
		}
		filter.Page = value
	}
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return EventFilter{}, infraerrors.BadRequest("PROMPT_AUDIT_PAGE_SIZE_INVALID", "page_size is invalid")
		}
		filter.PageSize = value
	}
	for raw, target := range map[string]**int64{"group_id": &filter.GroupID, "user_id": &filter.UserID} {
		if value := strings.TrimSpace(c.Query(raw)); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed <= 0 {
				return EventFilter{}, infraerrors.BadRequest("PROMPT_AUDIT_FILTER_INVALID", "event filter is invalid")
			}
			copyValue := parsed
			*target = &copyValue
		}
	}
	return filter, nil
}
