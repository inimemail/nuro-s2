package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWriteUpstreamResponseRejectsHTMLAndProviderHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	writeUpstreamResponse(c, &service.UpstreamHTTPResult{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":     {"text/html"},
			"Server":           {"cloudflare"},
			"Location":         {"https://private.example/error"},
			"Www-Authenticate": {`Bearer realm="xAI"`},
		},
		Body: []byte(`<!DOCTYPE html><title>private.example | 502</title>`),
	})

	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.Contains(t, recorder.Body.String(), "Upstream request failed")
	require.NotContains(t, recorder.Body.String(), "private.example")
	require.Empty(t, recorder.Header().Get("Server"))
	require.Empty(t, recorder.Header().Get("Location"))
	require.Empty(t, recorder.Header().Get("Www-Authenticate"))
}

func TestWriteUpstreamResponseFiltersSuccessfulHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	writeUpstreamResponse(c, &service.UpstreamHTTPResult{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":     {"application/json"},
			"X-Request-Id":     {"request-1"},
			"Server":           {"private-provider"},
			"Www-Authenticate": {`Bearer realm="private-provider"`},
		},
		Body: []byte(`{"models":[]}`),
	})

	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"models":[]}`, recorder.Body.String())
	require.Equal(t, responseheaders.PublicRequestID("request-1"), recorder.Header().Get("X-Request-Id"))
	require.Empty(t, recorder.Header().Get("Server"))
	require.Empty(t, recorder.Header().Get("Www-Authenticate"))
}

func TestWriteUpstreamResponseRejectsRedirectAndErrorEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "redirect", statusCode: http.StatusFound, body: `{"models":[]}`},
		{name: "error envelope", statusCode: http.StatusOK, body: `{"error":{"message":"private.example"}}`},
		{name: "diagnostic envelope", statusCode: http.StatusOK, body: `{"message":"private.example failed"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			writeUpstreamResponse(c, &service.UpstreamHTTPResult{
				StatusCode: tt.statusCode,
				Headers:    http.Header{"Location": {"https://private.example/error"}},
				Body:       []byte(tt.body),
			})

			require.Equal(t, http.StatusBadGateway, recorder.Code)
			require.Contains(t, recorder.Body.String(), "Upstream request failed")
			require.NotContains(t, recorder.Body.String(), "private.example")
			require.Empty(t, recorder.Header().Get("Location"))
		})
	}
}
