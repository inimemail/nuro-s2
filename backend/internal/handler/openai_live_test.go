package handler

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestParseLiveCallRequestPreservesJSONAndMultipartSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	session := `{"model":"gpt-live-test","custom":{"keep":true}}`

	t.Run("json", func(t *testing.T) {
		body := `{"sdp":"v=0\\r\\n","session":` + session + `}`
		request := httptest.NewRequest(http.MethodPost, "/v1/live", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = request

		parsed, err := parseLiveCallRequest(ctx)
		require.NoError(t, err)
		require.JSONEq(t, session, string(parsed.Session))
	})

	t.Run("multipart", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		require.NoError(t, writer.WriteField("sdp", "v=0\r\n"))
		require.NoError(t, writer.WriteField("session", session))
		require.NoError(t, writer.Close())
		request := httptest.NewRequest(http.MethodPost, "/backend-api/codex/realtime/calls", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = request

		parsed, err := parseLiveCallRequest(ctx)
		require.NoError(t, err)
		require.Equal(t, "v=0\r\n", parsed.SDP)
		require.JSONEq(t, session, string(parsed.Session))
	})
}

func TestParseLiveCallRequestRejectsInvalidShapesAndTrailingJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		`{"session":{"model":"gpt-live"}}`,
		`{"sdp":"v=0","session":[]}`,
		`{"sdp":"v=0","session":null}`,
		`{"sdp":"v=0","session":{"model":"gpt-live"}} {}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/live", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = request
		_, err := parseLiveCallRequest(ctx)
		require.Error(t, err)
	}
}

func TestLiveEnabledForAPIKeyIsOpenAIGroupScoped(t *testing.T) {
	require.False(t, liveEnabledForAPIKey(nil))
	require.False(t, liveEnabledForAPIKey(&service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}}))
	require.False(t, liveEnabledForAPIKey(&service.APIKey{Group: &service.Group{Platform: service.PlatformAnthropic, AllowLive: true}}))
	require.True(t, liveEnabledForAPIKey(&service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI, AllowLive: true}}))
}

func TestLiveAttestationErrorDoesNotExposeEnvironmentReason(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	(&OpenAIGatewayHandler{}).writeLiveCreateError(ctx, &service.LiveAttestationUnavailableError{
		Reason: "internal platform and app diagnostics",
	})

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.NotContains(t, recorder.Body.String(), "internal platform")
	require.Contains(t, recorder.Body.String(), "Live is unavailable")
}

func TestLiveSidebandLocationStaysWithinSelectedProtocol(t *testing.T) {
	require.Equal(t, "/v1/live/call_123", liveSidebandLocation("/v1/live", "call_123"))
	require.Equal(t, "/backend-api/codex/call_123", liveSidebandLocation("/backend-api/codex/realtime/calls", "call_123"))
}
