package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"strconv"
)

func (h *AccountHandler) GetOpenCodeGoUsageSettings(c *gin.Context) {
	if h.openCodeGoUsage == nil {
		response.Error(c, 503, "usage service unavailable")
		return
	}
	value, err := h.openCodeGoUsage.GetSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, value)
}
func (h *AccountHandler) UpdateOpenCodeGoUsageSettings(c *gin.Context) {
	var value service.OpenCodeGoUsageSettings
	if h.openCodeGoUsage == nil || c.ShouldBindJSON(&value) != nil {
		response.BadRequest(c, "invalid usage settings")
		return
	}
	if err := h.openCodeGoUsage.UpdateSettings(c.Request.Context(), value); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, value)
}
func (h *AccountHandler) OpenCodeGoUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 || h.openCodeGoUsage == nil {
		response.BadRequest(c, "invalid account")
		return
	}
	var state *service.OpenCodeGoUsageState
	switch c.Request.Method {
	case "PATCH":
		var input struct {
			AutoRefresh bool   `json:"auto_refresh"`
			Source      string `json:"source"`
		}
		if c.ShouldBindJSON(&input) != nil {
			response.BadRequest(c, "invalid usage configuration")
			return
		}
		state, err = h.openCodeGoUsage.Configure(c.Request.Context(), id, input.AutoRefresh, input.Source)
	case "POST":
		state, err = h.openCodeGoUsage.Refresh(c.Request.Context(), id)
	default:
		state, err = h.openCodeGoUsage.GetState(c.Request.Context(), id)
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}
