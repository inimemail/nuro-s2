package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newGatewayRoutesTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	RegisterGatewayRoutes(
		router,
		&handler.Handlers{
			Gateway:       &handler.GatewayHandler{},
			OpenAIGateway: &handler.OpenAIGatewayHandler{},
		},
		servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
			groupID := int64(1)
			c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{
				GroupID: &groupID,
				Group:   &service.Group{Platform: service.PlatformOpenAI},
			})
			c.Next()
		}),
		nil,
		nil,
		nil,
		nil,
		&config.Config{},
	)

	return router
}

func TestGatewayRoutesOpenAIResponsesCompactPathIsRegistered(t *testing.T) {
	router := newGatewayRoutesTestRouter()

	for _, path := range []string{
		"/v1/responses/compact",
		"/responses/compact",
		"/backend-api/codex/responses",
		"/backend-api/codex/responses/compact",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-5"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code, "path=%s should hit OpenAI responses handler", path)
	}
}

func TestGatewayPlatformRoutingKeepsMessagesScopeNarrow(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformGrok} {
		require.True(t, isOpenAIMessagesGatewayPlatform(platform), platform)
	}
	for _, platform := range []string{service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepSeek, service.PlatformAnthropic} {
		require.False(t, isOpenAIMessagesGatewayPlatform(platform), platform)
	}
}

func TestGatewayPlatformRoutingSupportsCNChatCompletions(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformGrok, service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepSeek} {
		require.True(t, isOpenAIChatCompletionsGatewayPlatform(platform), platform)
	}
	require.False(t, isOpenAIChatCompletionsGatewayPlatform(service.PlatformAnthropic))
}

func TestGatewayRoutesCodexModelsManifestPathIsRegistered(t *testing.T) {
	router := newGatewayRoutesTestRouter()

	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		if route.Method == http.MethodGet {
			registered[route.Path] = true
		}
	}

	require.True(t, registered["/backend-api/codex/models"], "GET /backend-api/codex/models should be registered")
	require.True(t, registered["/v1/models"], "GET /v1/models should be registered")
	require.True(t, registered["/models"], "GET /models should be registered")
}

func TestGatewayRoutesOpenAIImagesPathsAreRegistered(t *testing.T) {
	router := newGatewayRoutesTestRouter()

	for _, path := range []string{
		"/v1/images/generations",
		"/v1/images/edits",
		"/images/generations",
		"/images/edits",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a cat"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code, "path=%s should hit OpenAI images handler", path)
	}
}

func TestGatewayRoutesOpenAIImageTaskPathsAreRegistered(t *testing.T) {
	router := newGatewayRoutesTestRouter()

	for _, path := range []string{
		"/v1/image-tasks",
		"/image-tasks",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code, "path=%s should hit OpenAI image tasks handler", path)
	}

	for _, path := range []string{
		"/v1/image-tasks/generations",
		"/v1/image-tasks/edits",
		"/image-tasks/generations",
		"/image-tasks/edits",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"client_task_id":"task-1","model":"gpt-image-2","prompt":"draw a cat"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code, "path=%s should hit OpenAI image tasks handler", path)
	}
}
