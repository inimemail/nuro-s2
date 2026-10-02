package middleware

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// rejectTypeSafeUnsupportedEndpoint prevents a TypeSafe API key from reaching
// compatibility handlers such as Responses WebSocket, Codex, or Chat. The
// native System One endpoint is deliberately kept separate from those
// protocols; read-only model/usage/billing endpoints remain available.
func rejectTypeSafeUnsupportedEndpoint(c *gin.Context, apiKey *service.APIKey, googleStyle bool) bool {
	if apiKey == nil || apiKey.Group == nil || apiKey.Group.Platform != service.PlatformTypeSafe {
		return false
	}
	path := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path = strings.TrimRight(c.Request.URL.Path, "/")
	}
	if typeSafeEndpointAllowed(path) {
		return false
	}
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
	if googleStyle {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{
			"code": http.StatusNotFound, "message": "This endpoint is not supported for TypeSafe", "status": "NOT_FOUND",
		}})
	} else {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"type": "error", "error": gin.H{
			"type": "not_found_error", "message": "This endpoint is not supported for TypeSafe",
		}})
	}
	return true
}

func typeSafeEndpointAllowed(path string) bool {
	switch path {
	case "/v1/systemone", "/v1/models", "/models", "/v1/usage", "/usage", "/v1/sub2api/billing":
		return true
	default:
		return strings.HasPrefix(path, "/v1/models/") || strings.HasPrefix(path, "/models/")
	}
}
