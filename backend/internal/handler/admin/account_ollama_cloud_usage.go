package admin

import (
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) GetOllamaCloudUsageSettings(c *gin.Context) {
	if h.ollamaCloudUsage == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return
	}
	settings, err := h.ollamaCloudUsage.GetSettings(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, settings)
}
func (h *AccountHandler) UpdateOllamaCloudUsageSettings(c *gin.Context) {
	var settings service.OllamaCloudUsageSettings
	if err := c.ShouldBindJSON(&settings); err != nil || h.ollamaCloudUsage == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid settings"})
		return
	}
	if err := h.ollamaCloudUsage.UpdateSettings(c, &settings); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, &settings)
}

func (h *AccountHandler) GetOllamaCloudUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || h.ollamaCloudUsage == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account"})
		return
	}
	state, err := h.ollamaCloudUsage.GetState(c, id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}
func (h *AccountHandler) SetOllamaCloudUsageSession(c *gin.Context) {
	var req struct {
		Session string `json:"session" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || h.ollamaCloudUsage == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account"})
		return
	}
	state, err := h.ollamaCloudUsage.SaveSession(c, id, req.Session)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}
func (h *AccountHandler) DeleteOllamaCloudUsageSession(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || h.ollamaCloudUsage == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account"})
		return
	}
	state, err := h.ollamaCloudUsage.DeleteSession(c, id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}
func (h *AccountHandler) SetOllamaCloudUsageAutoRefresh(c *gin.Context) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || h.ollamaCloudUsage == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account"})
		return
	}
	state, err := h.ollamaCloudUsage.SetAutoRefresh(c, id, req.Enabled)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}
func (h *AccountHandler) RefreshOllamaCloudUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || h.ollamaCloudUsage == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account"})
		return
	}
	state, err := h.ollamaCloudUsage.Refresh(c, id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}
