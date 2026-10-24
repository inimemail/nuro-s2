//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpgradeGeminiImageGateRejectsBeforeAccountSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, entry := range []string{"messages", "chat", "responses", "native-model", "native-modalities"} {
		t.Run(entry, func(t *testing.T) {
			groupID := int64(7)
			group := &service.Group{ID: groupID, Platform: service.PlatformGemini, Status: service.StatusActive, Hydrated: true}
			// No schedulers, credentials, concurrency helpers or upstreams are
			// installed: reaching any of them would fail this test.
			h := &GatewayHandler{gatewayService: &service.GatewayService{}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := `{"model":"gemini-3-pro-image","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"input":"hello"}`
			if strings.HasPrefix(entry, "native") {
				model := "gemini-3-pro-image"
				if entry == "native-modalities" {
					model = "gemini-text"
				}
				c.Params = gin.Params{{Key: "modelAction", Value: model + ":generateContent"}}
				body = `{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"responseModalities":["IMAGE"]}}`
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			key := &service.APIKey{ID: 1, UserID: 2, GroupID: &groupID, Group: group}
			c.Set(string(middleware.ContextKeyAPIKey), key)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 1})
			switch entry {
			case "messages":
				h.Messages(c)
			case "chat":
				h.ChatCompletions(c)
			case "responses":
				h.Responses(c)
			default:
				h.GeminiV1BetaModels(c)
			}
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), service.ImageGenerationPermissionMessage())
			_, selected := c.Get(opsAccountIDKey)
			require.False(t, selected)
		})
	}
}

func TestUpgradeGeminiImageGateCountTokensExempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(7)
	group := &service.Group{ID: groupID, Platform: service.PlatformGemini, Status: service.StatusActive, Hydrated: true}
	h, cleanup := newTestGatewayHandler(t, group, nil)
	defer cleanup()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "modelAction", Value: "gemini-3-pro-image:countTokens"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"responseModalities":["IMAGE"]}}`))
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, UserID: 2, GroupID: &groupID, Group: group})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 1})
	h.GeminiV1BetaModels(c)
	// With no accounts the request reaches ordinary capacity handling, which
	// proves token counting did not fail the local image permission gate.
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.NotContains(t, rec.Body.String(), service.ImageGenerationPermissionMessage())
}

func TestUpgradeVertexQuotaDTOIsolation(t *testing.T) {
	for _, platform := range []string{service.PlatformGemini, service.PlatformAnthropic} {
		a := &service.Account{Platform: platform, Type: service.AccountTypeServiceAccount,
			Extra: map[string]any{"quota_limit": 20.0, "quota_used": 3.0, "quota_daily_limit": 5.0, "quota_weekly_limit": 12.0}}
		out := dto.AccountFromServiceShallow(a)
		if platform == service.PlatformGemini {
			require.NotNil(t, out.QuotaLimit)
			require.Equal(t, 20.0, *out.QuotaLimit)
			require.Equal(t, 3.0, *out.QuotaUsed)
			require.Equal(t, 5.0, *out.QuotaDailyLimit)
			require.Equal(t, 12.0, *out.QuotaWeeklyLimit)
		} else {
			require.Nil(t, out.QuotaLimit)
			require.Nil(t, out.QuotaDailyLimit)
			require.Nil(t, out.QuotaWeeklyLimit)
		}
	}
}
