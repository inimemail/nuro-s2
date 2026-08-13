package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"net/http"
	"strconv"
)

type CompositeRouteHandler struct {
	resolver *service.CompositeRouteResolver
}

func NewCompositeRouteHandler(resolver *service.CompositeRouteResolver) *CompositeRouteHandler {
	return &CompositeRouteHandler{resolver: resolver}
}

type compositeRouteRequest struct {
	PublicModel    string `json:"public_model"`
	MatchType      string `json:"match_type"`
	TargetPlatform string `json:"target_platform"`
	UpstreamModel  string `json:"upstream_model"`
	Endpoint       string `json:"endpoint"`
	Priority       int    `json:"priority"`
	Enabled        bool   `json:"enabled"`
	Notes          string `json:"notes"`
}

func (h *CompositeRouteHandler) groupID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid group id"})
		return 0, false
	}
	return id, true
}
func (h *CompositeRouteHandler) List(c *gin.Context) {
	id, ok := h.groupID(c)
	if !ok {
		return
	}
	rows, err := h.resolver.List(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rows)
}
func (h *CompositeRouteHandler) Preview(c *gin.Context) {
	id, ok := h.groupID(c)
	if !ok {
		return
	}
	var req service.CompositeRoutePreviewRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	d, err := h.resolver.Resolve(c.Request.Context(), id, req.Model, req.Endpoint)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, d)
}
func (h *CompositeRouteHandler) Create(c *gin.Context) {
	id, ok := h.groupID(c)
	if !ok {
		return
	}
	var req compositeRouteRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	row, err := h.resolver.CreateRoute(c.Request.Context(), id, service.CompositeRouteInput{PublicModel: req.PublicModel, MatchType: req.MatchType, TargetPlatform: req.TargetPlatform, UpstreamModel: req.UpstreamModel, Endpoint: req.Endpoint, Priority: req.Priority, Enabled: req.Enabled, Notes: req.Notes})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, row)
}
func (h *CompositeRouteHandler) Update(c *gin.Context) {
	groupID, ok := h.groupID(c)
	if !ok {
		return
	}
	rid, err := strconv.ParseInt(c.Param("route_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid route id"})
		return
	}
	var req compositeRouteRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	row, err := h.resolver.UpdateRouteForGroup(c.Request.Context(), groupID, rid, service.CompositeRouteInput{PublicModel: req.PublicModel, MatchType: req.MatchType, TargetPlatform: req.TargetPlatform, UpstreamModel: req.UpstreamModel, Endpoint: req.Endpoint, Priority: req.Priority, Enabled: req.Enabled, Notes: req.Notes})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, row)
}
func (h *CompositeRouteHandler) Delete(c *gin.Context) {
	groupID, ok := h.groupID(c)
	if !ok {
		return
	}
	rid, err := strconv.ParseInt(c.Param("route_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid route id"})
		return
	}
	if err := h.resolver.DeleteRouteForGroup(c.Request.Context(), groupID, rid); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}
