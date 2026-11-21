package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestEdgePrepareFieldPolicyBeforeLease(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.MaxBodySize = 1024
	cfg.Gateway.OpenAIEdgeRS.InternalAPIEnabled = true
	cfg.Gateway.OpenAIEdgeRS.InternalSecret = "local-fixture"
	handler := &OpenAIGatewayHandler{cfg: cfg}
	api := gin.New()
	reached := 0
	api.POST("/prepare", handler.ValidateOpenAIEdgePrepareFields, func(c *gin.Context) {
		reached++
		var request service.OpenAIEdgePrepareRequest
		require.NoError(t, c.ShouldBindJSON(&request))
		require.Equal(t, "fixture", request.EdgeRequestID)
		c.Status(http.StatusOK)
	})

	for _, test := range []struct {
		name, secret, clientBody string
		want                     int
	}{
		{"missing internal secret", "", `{"model":"fixture"}`, http.StatusUnauthorized},
		{"conflicting raw WS fields", "local-fixture", `{"model":"fixture","model":"fixture"}`, http.StatusBadRequest},
		{"oversized decoded client body", "local-fixture", strings.Repeat(" ", 1025), http.StatusRequestEntityTooLarge},
		{"valid client fields", "local-fixture", `{"model":"fixture"}`, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(service.OpenAIEdgePrepareRequest{
				EdgeRequestID: "fixture", BodyRawBase64: base64.StdEncoding.EncodeToString([]byte(test.clientBody)),
			})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/prepare", strings.NewReader(string(body)))
			request.Header.Set(openAIEdgeSecretHeader, test.secret)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			api.ServeHTTP(recorder, request)
			require.Equal(t, test.want, recorder.Code)
		})
	}
	require.Equal(t, 1, reached)
}
