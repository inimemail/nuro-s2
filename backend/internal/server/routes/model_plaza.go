package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func RegisterModelPlazaRoutes(v1 *gin.RouterGroup, h *handler.Handlers, jwtAuth middleware.JWTAuthMiddleware, settingService *service.SettingService, panelRateLimiter *middleware.PanelRateLimiter) {
	plaza := v1.Group("/model-plaza")
	plaza.Use(panelRateLimiter.PublicIP())
	plaza.Use(middleware.OptionalJWTAuth(jwtAuth))
	plaza.Use(middleware.BackendModeUserGuard(settingService))
	plaza.GET("", h.ModelPlaza.Get)
}
