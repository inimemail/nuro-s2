package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorRequestsAcceptGrok(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("create monitor uses default model", func(t *testing.T) {
		var bound channelMonitorCreateRequest
		status := bindChannelMonitorTestRequest(t, http.MethodPost, `{
			"name":"grok",
			"provider":"grok",
			"endpoint":"https://api.x.ai",
			"api_key":"xai-test-key",
			"interval_seconds":60
		}`, func(c *gin.Context) error {
			return c.ShouldBindJSON(&bound)
		})

		require.Equal(t, http.StatusNoContent, status)
		require.Equal(t, "grok", bound.Provider)
		require.Empty(t, bound.PrimaryModel)
	})

	t.Run("update monitor provider", func(t *testing.T) {
		var bound channelMonitorUpdateRequest
		status := bindChannelMonitorTestRequest(t, http.MethodPut, `{"provider":"grok"}`, func(c *gin.Context) error {
			return c.ShouldBindJSON(&bound)
		})

		require.Equal(t, http.StatusNoContent, status)
		require.NotNil(t, bound.Provider)
		require.Equal(t, "grok", *bound.Provider)
	})

	t.Run("create template", func(t *testing.T) {
		var bound channelMonitorTemplateCreateRequest
		status := bindChannelMonitorTestRequest(t, http.MethodPost, `{"name":"grok","provider":"grok"}`, func(c *gin.Context) error {
			return c.ShouldBindJSON(&bound)
		})

		require.Equal(t, http.StatusNoContent, status)
		require.Equal(t, "grok", bound.Provider)
	})
}

func bindChannelMonitorTestRequest(t *testing.T, method, body string, bind func(*gin.Context) error) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if err := bind(c); err != nil {
		c.Status(http.StatusBadRequest)
	} else {
		c.Status(http.StatusNoContent)
	}
	return c.Writer.Status()
}
