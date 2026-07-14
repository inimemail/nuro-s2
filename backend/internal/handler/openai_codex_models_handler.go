package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// CodexModels serves the Codex models manifest for Codex clients.
func (h *OpenAIGatewayHandler) CodexModels(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"type":    "upstream_error",
				"message": "Service temporarily unavailable",
			},
		})
		return
	}
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey.Group == nil {
		h.errorResponse(c, http.StatusUnauthorized, "invalid_request_error", "API key group is required")
		return
	}
	if apiKey.Group.Platform != service.PlatformOpenAI {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Models manifest is not available for this group")
		return
	}
	if h.gatewayService == nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "upstream_error", "Service temporarily unavailable")
		return
	}

	account, err := h.gatewayService.SelectCodexModelsManifestAccount(c.Request.Context(), apiKey.GroupID)
	if err != nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "upstream_error", "Service temporarily unavailable")
		return
	}

	manifest, err := h.gatewayService.FetchCodexModelsManifest(c.Request.Context(), account, c.Query("client_version"), c.GetHeader("If-None-Match"))
	if err != nil {
		status := infraerrors.Code(err)
		if status < http.StatusBadRequest || status > 599 {
			status = http.StatusBadGateway
		}
		h.errorResponse(c, status, "upstream_error", "Service temporarily unavailable")
		return
	}

	if manifest.ETag != "" {
		c.Header("ETag", manifest.ETag)
	}
	if manifest.NotModified {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json", manifest.Body)
}
