package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestV0222AnthropicCompatibilityPolicyDeniesBeforeSelection(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			body := `{"model":"claude-sonnet-4-6","reasoning_effort":"high","messages":[]}`
			if protocol == "responses" {
				body = `{"model":"claude-sonnet-4-6","reasoning":{"effort":"high"},"input":[]}`
			}
			c.Request = httptest.NewRequest("POST", "/v1/"+protocol, strings.NewReader(body))
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, Group: &service.Group{Platform: service.PlatformAnthropic, MaxReasoningEffort: "medium", MaxReasoningEffortOverLimit: "deny"}})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
			// No scheduler or upstream is configured: a deny must return before either.
			h := &GatewayHandler{}
			if protocol == "chat" {
				h.ChatCompletions(c)
			} else {
				h.Responses(c)
			}
			require.Equal(t, 400, w.Code)
			require.NotNil(t, service.RequestedReasoningEffortFromContext(c.Request.Context()))
			require.Equal(t, "high", *service.RequestedReasoningEffortFromContext(c.Request.Context()))
		})
	}
}
