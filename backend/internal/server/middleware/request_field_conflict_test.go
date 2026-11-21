package middleware

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayFieldCheckLimitsDecodedBody(t *testing.T) {
	const limit = 128
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	_, err := gzipWriter.Write([]byte(strings.Repeat("x", limit+1)))
	require.NoError(t, err)
	require.NoError(t, gzipWriter.Close())
	require.Less(t, compressed.Len(), limit)

	router := gin.New()
	router.POST("/v1/responses", RequestBodyLimit(limit), RejectConflictingGatewayFields(limit), func(c *gin.Context) {
		t.Error("oversized decoded body reached handler")
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", &compressed)
	request.Header.Set("Content-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
}

func TestGatewayFieldConflictMiddleware(t *testing.T) {
	router := gin.New()
	const maxBytes = 64
	checked := RejectConflictingGatewayFields(maxBytes)
	received := 0
	router.POST("/*path", RequestBodyLimit(maxBytes), checked, func(c *gin.Context) {
		received++
		body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		require.NoError(t, err)
		c.Data(http.StatusOK, "application/json", body)
	})
	cases := []struct {
		name, path, input string
		status            int
		want              string
	}{
		{"valid", "/v1/responses", `{"model":"fixture","input":"check"}`, 200, `{"model":"fixture","input":"check"}`},
		{"ambiguous", "/v1/responses", `{"model":"fixture","model":"fixture"}`, 400, `"invalid_request_error"`},
		{"messages", "/v1/messages", `{"model":"fixture","model":"fixture"}`, 400, `"type":"error"`},
		{"gemini", "/v1beta/models/fixture:generateContent", `{"contents":[],"contents":[]}`, 400, `"status":"INVALID_ARGUMENT"`},
		{"size", "/v1/responses", strings.Repeat(" ", maxBytes+1), 413, `"Request body is too large"`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.input))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			require.Equal(t, test.status, recorder.Code)
			require.Contains(t, recorder.Body.String(), test.want)
		})
	}
	require.Equal(t, 1, received)
}
