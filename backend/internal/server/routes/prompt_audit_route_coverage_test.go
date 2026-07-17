package routes

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayPostRoutesHavePromptAuditCoverageOrExplicitNoPromptReason(t *testing.T) {
	source, err := os.ReadFile("gateway.go")
	require.NoError(t, err)
	pattern := regexp.MustCompile(`(?:gateway|gemini|r|codexDirect|antigravityV1|antigravityV1Beta)\.POST\("([^"]+)"`)
	actual := map[string]struct{}{}
	for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
		actual[match[1]] = struct{}{}
	}
	// These handlers either register the collector through the common
	// moderation helper or capture their structured prompt before submission.
	audited := map[string][]string{
		"/messages":                 {"gateway_handler.go", "openai_gateway_handler.go"},
		"/responses":                {"gateway_handler_responses.go", "openai_gateway_handler.go"},
		"/responses/*subpath":       {"gateway_handler_responses.go", "openai_gateway_handler.go"},
		"/chat/completions":         {"gateway_handler_chat_completions.go", "openai_chat_completions.go"},
		"/embeddings":               {"openai_embeddings.go"},
		"/alpha/search":             {"openai_alpha_search.go"},
		"/images/generations":       {"openai_images.go", "grok_media.go"},
		"/images/edits":             {"openai_images.go", "grok_media.go"},
		"/images/generations/async": {"openai_image_tasks.go"},
		"/images/edits/async":       {"openai_image_tasks.go"},
		"/image-tasks/generations":  {"openai_image_tasks.go"},
		"/image-tasks/edits":        {"openai_image_tasks.go"},
		"/images/batches":           {"batch_image_handler.go"},
		"/videos/generations":       {"grok_media.go"},
		"/videos/edits":             {"grok_media.go"},
		"/videos/extensions":        {"grok_media.go"},
		"/models/*modelAction":      {"gemini_v1beta_handler.go"},
	}
	excluded := map[string]string{
		"/messages/count_tokens":     "tokenization only; it does not execute a model request",
		"/images/batches/:id/cancel": "control-plane cancellation with no user prompt",
	}
	var unclassified []string
	for route := range actual {
		if _, ok := audited[route]; ok {
			continue
		}
		if _, ok := excluded[route]; ok {
			continue
		}
		unclassified = append(unclassified, route)
	}
	sort.Strings(unclassified)
	require.Empty(t, unclassified)
	for route, files := range audited {
		require.Contains(t, actual, route)
		for _, filename := range files {
			file, readErr := os.ReadFile(filepath.Join("..", "..", "handler", filename))
			require.NoError(t, readErr)
			text := string(file)
			require.Truef(t, strings.Contains(text, "checkContentModeration") || strings.Contains(text, "capturePromptAudit"), "%s lacks audit capture", filename)
		}
	}
}

func TestPromptAuditAdminRoutesAreProtected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{PromptAudit: securityaudit.NewPromptAdminHandler(nil)}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
			return
		}
		c.Next()
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) {
		servermiddleware.AbortWithError(c, http.StatusUnauthorized, "STEP_UP_REQUIRED", "step-up authentication required")
	})
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp)

	for _, tc := range []struct {
		name, method, path, auth string
		want                     int
	}{
		{name: "config unauthenticated", method: http.MethodGet, path: "/api/v1/admin/prompt-audit/config", want: http.StatusUnauthorized},
		{name: "config update step up", method: http.MethodPut, path: "/api/v1/admin/prompt-audit/config", auth: "Bearer admin", want: http.StatusUnauthorized},
		{name: "probe step up", method: http.MethodPost, path: "/api/v1/admin/prompt-audit/probe", auth: "Bearer admin", want: http.StatusUnauthorized},
		{name: "upstream endpoint probe step up", method: http.MethodPost, path: "/api/v1/admin/prompt-audit/endpoints/probe", auth: "Bearer admin", want: http.StatusUnauthorized},
		{name: "runtime admin", method: http.MethodGet, path: "/api/v1/admin/prompt-audit/runtime", auth: "Bearer admin", want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code)
		})
	}
}
