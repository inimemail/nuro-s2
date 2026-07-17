package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestS3ConnectionTestRoutesRequireStepUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stepUpCalls := 0
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) {
		stepUpCalls++
		c.AbortWithStatus(http.StatusTeapot)
	})
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		Backup:         &adminhandler.BackupHandler{},
		DataManagement: &adminhandler.DataManagementHandler{},
	}}

	router := gin.New()
	admin := router.Group("/api/v1/admin")
	registerBackupRoutes(admin, handlers, stepUp)
	registerDataManagementRoutes(admin, handlers, stepUp)

	for _, path := range []string{
		"/api/v1/admin/backups/s3-config/test",
		"/api/v1/admin/data-management/s3/test",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		require.Equal(t, http.StatusTeapot, recorder.Code, path)
	}
	require.Equal(t, 2, stepUpCalls)
}
