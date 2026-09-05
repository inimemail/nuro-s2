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
	if c.Request.Context().Err() != nil {
		return
	}
	if h == nil || h.gatewayService == nil {
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
	if cfg := apiKey.Group.CodexModelsManifestConfig; cfg.Enabled && len(cfg.AccountIDs) > 0 {
		manifest, account, err := h.gatewayService.FetchPinnedCodexModelsManifest(c.Request.Context(), apiKey.Group, c.Query("client_version"))
		if err == nil && manifest != nil {
			if account != nil {
				setOpsSelectedAccount(c, account.ID, account.Platform)
			}
			if manifest.ETag != "" {
				c.Header("ETag", manifest.ETag)
			}
			if service.CodexModelsManifestETagMatches(c.GetHeader("If-None-Match"), manifest.ETag) {
				c.Status(http.StatusNotModified)
				return
			}
			c.Data(http.StatusOK, "application/json", manifest.Body)
			return
		}
		if !cfg.FallbackToScheduler {
			h.errorResponse(c, http.StatusServiceUnavailable, "upstream_error", "No available pinned OpenAI accounts")
			return
		}
	}
	maxAccountSwitches := h.maxAccountSwitches
	if maxAccountSwitches <= 0 {
		maxAccountSwitches = 3
	}
	failedAccountIDs := make(map[int64]struct{})
	switchCount := 0
	var lastUpstreamErr error

	for {
		account, err := h.gatewayService.SelectCodexModelsManifestAccountWithExclusions(c.Request.Context(), apiKey.GroupID, failedAccountIDs)
		if err != nil {
			if c.Request.Context().Err() != nil {
				return
			}
			status := http.StatusServiceUnavailable
			if lastUpstreamErr != nil {
				status = infraerrors.Code(lastUpstreamErr)
			}
			if status < http.StatusBadRequest || status > 599 {
				status = http.StatusBadGateway
			}
			h.errorResponse(c, status, "upstream_error", "Service temporarily unavailable")
			return
		}
		setOpsSelectedAccount(c, account.ID, account.Platform)

		manifest, err := h.gatewayService.FetchCodexModelsManifest(c.Request.Context(), account, c.Query("client_version"), c.GetHeader("If-None-Match"))
		if err != nil {
			if c.Request.Context().Err() != nil {
				return
			}
			if service.IsRetryableCodexModelsManifestError(err) && switchCount < maxAccountSwitches {
				failedAccountIDs[account.ID] = struct{}{}
				switchCount++
				lastUpstreamErr = err
				continue
			}
			status := infraerrors.Code(err)
			if status < http.StatusBadRequest || status > 599 {
				status = http.StatusBadGateway
			}
			h.errorResponse(c, status, "upstream_error", "Service temporarily unavailable")
			return
		}
		if c.Request.Context().Err() != nil {
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
		return
	}
}
