package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

// RegisterOpenAIEdgeRoutes exposes the local-only control-plane API used by
// sub2api-edge-rs. It is disabled unless gateway.openai_edge_rs.internal_api_enabled
// is explicitly true and protected again by a shared secret in the handler.
func RegisterOpenAIEdgeRoutes(r *gin.Engine, h *handler.Handlers, cfg *config.Config) {
	if r == nil || h == nil || h.OpenAIGateway == nil || cfg == nil {
		return
	}
	if !cfg.Gateway.OpenAIEdgeRS.InternalAPIEnabled {
		return
	}
	edge := r.Group("/internal/edge/openai")
	{
		edge.POST("/prepare", h.OpenAIGateway.OpenAIEdgePrepare)
		edge.POST("/retry", h.OpenAIGateway.OpenAIEdgeRetry)
		edge.POST("/complete", h.OpenAIGateway.OpenAIEdgeComplete)
		edge.POST("/abort", h.OpenAIGateway.OpenAIEdgeAbort)
		edge.POST("/recover", h.OpenAIGateway.OpenAIEdgeRecover)
	}
}
