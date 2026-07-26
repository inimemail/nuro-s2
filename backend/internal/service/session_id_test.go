package service

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestExtractClientSessionID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		header string
		value  string
		want   string
	}{
		{name: "session", header: "session_id", value: " session-42 ", want: "session-42"},
		{name: "conversation", header: "conversation_id", value: "conv-7", want: "conv-7"},
		{name: "controls rejected", header: "X-Session-Id", value: "bad\nvalue"},
		{name: "too long rejected", header: "X-Session-Id", value: strings.Repeat("x", maxPersistedSessionIDLength+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/responses", nil)
			req.Header.Set(tt.header, tt.value)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = req
			require.Equal(t, tt.want, ExtractClientSessionID(ctx))
		})
	}
}

func TestExtractClientSessionIDDoesNotUseCacheKey(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("prompt_cache_key", "sticky-cache-key")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req
	require.Empty(t, ExtractClientSessionID(ctx))
}

func TestBatchImageHashIgnoresAuditSessionID(t *testing.T) {
	request := BatchImageSubmitRequest{Model: "imagen", Items: []BatchImageSubmitItem{{Prompt: "hello"}}}
	baseline := HashBatchImageSubmitRequest(request)
	request.SessionID = "session-42"
	require.Equal(t, baseline, HashBatchImageSubmitRequest(request))
}
