package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSanitizedUpstreamPathSuffix(t *testing.T) {
	for _, suffix := range []string{"/..", "/../x", `/a\\b`, "/a?b", "/a#b", "/a//b", "/...", "/模型", "/a\x00b"} {
		_, ok := sanitizedUpstreamPathSuffix(suffix)
		require.False(t, ok, suffix)
	}
	for _, suffix := range []string{"", "/compact", "/response_1/cancel", "/gemini-2.5-pro_v1.2"} {
		got, ok := sanitizedUpstreamPathSuffix(suffix)
		require.True(t, ok, suffix)
		require.Equal(t, suffix, got)
	}
}

func TestForwardableOpenAIResponsesRequestPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		path string
		want bool
	}{
		{path: "/v1/responses/resp_123/cancel", want: true},
		{path: "/v1/responses/resp_123/compact/", want: true},
		{path: "/v1/responses/../admin", want: false},
		{path: "/v1/responses/resp_123/%2e%2e", want: false},
	} {
		t.Run(test.path, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, test.path, nil)
			require.Equal(t, test.want, IsForwardableOpenAIResponsesRequestPath(c))
		})
	}
}
