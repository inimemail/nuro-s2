package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGrokTestErrorCategoryIsStableAndRedacted(t *testing.T) {
	tests := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, `{"error":"Bearer secret-token"}`, "token invalid"},
		{http.StatusForbidden, `{"error":"denied"}`, "permission denied"},
		{http.StatusNotFound, `{"error":"model not found"}`, "model not found"},
		{http.StatusNotFound, `{"error":"route missing"}`, "endpoint unsupported"},
		{http.StatusTooManyRequests, `{"error":"slow down"}`, "rate limited"},
		{http.StatusBadRequest, `{"error":"unknown parameter"}`, "unsupported request field"},
		{http.StatusServiceUnavailable, `{"error":"temporary"}`, "service unavailable"},
	}
	for _, tc := range tests {
		require.Equal(t, tc.want, grokTestErrorCategory(tc.status, []byte(tc.body)))
		require.NotContains(t, grokTestErrorCategory(tc.status, []byte(tc.body)), "secret-token")
	}
}

func TestGrokAccountTestUsesMatchingProtocolAndMediaPayload(t *testing.T) {
	image := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("test image"))
	audio := "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString([]byte("test audio"))
	for _, tc := range []struct {
		name, mode, model, response, contentType, path string
		media                                          AccountTestMedia
	}{
		{"chat", "chat", "grok-4.6", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", "text/event-stream", "/v1/chat/completions", AccountTestMedia{}},
		{"responses", "text", "grok-4.6", "data: {\"type\":\"response.completed\"}\n\n", "text/event-stream", "/v1/responses", AccountTestMedia{}},
		{"null_error_search", "search", "grok-4.6", `{"status":"completed","output":[],"error":null}`, "application/json", "/v1/responses", AccountTestMedia{}},
		{"default_image", "default", "grok-imagine-image", `{"data":[{"b64_json":"aGVsbG8="}]}`, "application/json", "/v1/images/generations", AccountTestMedia{}},
		{"search", "search", "grok-4.6", `{"status":"completed","output":[]}`, "application/json", "/v1/responses", AccountTestMedia{}},
		{"image", "image", "grok-imagine", `{"data":[{"b64_json":"aGVsbG8="}]}`, "application/json", "/v1/images/generations", AccountTestMedia{}},
		{"image_edit", "image", "grok-imagine", `{"data":[{"b64_json":"aGVsbG8="}]}`, "application/json", "/v1/images/edits", AccountTestMedia{ImageDataURL: image}},
		{"video", "video", "grok-imagine-video", `{"video":{"url":"https://example.com/video.mp4"}}`, "application/json", "/v1/videos/generations", AccountTestMedia{ImageDataURL: image}},
		{"tts", "tts", "grok-4.6", "audio bytes", "audio/mpeg", "/v1/tts", AccountTestMedia{}},
		{"stt", "stt", "grok-4.6", `{"text":"hello"}`, "application/json", "/v1/stt", AccountTestMedia{AudioDataURL: audio}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.response))}}
			svc := &AccountTestService{httpUpstream: upstream, cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-only-key"}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
			err := svc.testGrokAccountConnection(c, account, tc.model, "keep this prompt", tc.mode, tc.media)
			require.NoError(t, err)
			require.Equal(t, tc.path, upstream.lastReq.URL.Path)
			require.Contains(t, recorder.Body.String(), `"success":true`)
			switch tc.mode {
			case "image", "video":
				require.Equal(t, "keep this prompt", gjson.GetBytes(upstream.lastBody, "prompt").String())
				if tc.media.ImageDataURL != "" {
					require.Equal(t, image, gjson.GetBytes(upstream.lastBody, "image.url").String())
					require.False(t, gjson.GetBytes(upstream.lastBody, "input_reference").Exists())
				}
			case "tts":
				require.Equal(t, "keep this prompt", gjson.GetBytes(upstream.lastBody, "text").String())
				require.Equal(t, "en", gjson.GetBytes(upstream.lastBody, "language").String())
				require.False(t, gjson.GetBytes(upstream.lastBody, "model").Exists())
				require.Contains(t, recorder.Body.String(), "data:audio/mpeg;base64,")
			case "stt":
				_, params, err := mime.ParseMediaType(upstream.lastReq.Header.Get("Content-Type"))
				require.NoError(t, err)
				form, err := multipart.NewReader(bytes.NewReader(upstream.lastBody), params["boundary"]).ReadForm(1 << 20)
				require.NoError(t, err)
				defer form.RemoveAll()
				require.Equal(t, []string{"grok-stt"}, form.Value["model"])
				require.Equal(t, "probe.mp3", form.File["file"][0].Filename)
			}
		})
	}
}

func TestGrokAccountTestRejectsInvalidModeAndUnsuccessfulMedia(t *testing.T) {
	for _, tc := range []struct {
		mode, model, response string
		noRequest             bool
	}{
		{"image", "grok-4.6", "{}", true},
		{"text", "grok-imagine-video", "{}", true},
		{"invalid", "", "{}", true},
		{"stt", "", "{}", true},
		{"image", "grok-imagine-image", "{}", false},
		{"video", "grok-imagine-video", `{"status":"failed"}`, false},
		{"tts", "", `{"error":{"message":"secret-upstream-detail"}}`, false},
		{"search", "grok-4.6", `{"status":"in_progress","output":[]}`, false},
	} {
		t.Run(tc.mode+tc.model, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.response))}}
			svc := &AccountTestService{httpUpstream: upstream, cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-only-key"}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
			require.Error(t, svc.testGrokAccountConnection(c, account, tc.model, "", tc.mode))
			require.NotContains(t, recorder.Body.String(), `"success":true`)
			require.NotContains(t, recorder.Body.String(), "secret-upstream-detail")
			if tc.noRequest {
				require.Empty(t, upstream.requests)
			}
		})
	}
}

func TestGrokBackgroundTestDefaultModeCallsResponses(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-only-key"}}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))}}
	svc := &AccountTestService{accountRepo: &nonOpenAIPoolProbeAccountRepo{account: account}, httpUpstream: upstream, cfg: &config.Config{}}
	result, err := svc.RunTestBackground(context.Background(), 1, "grok-4.6")
	require.NoError(t, err)
	require.Equal(t, "success", result.Status)
	require.Equal(t, "/v1/responses", upstream.lastReq.URL.Path)
}

func TestGrokTestMediaValidation(t *testing.T) {
	for _, invalid := range []string{
		"", "https://example.com/file", "data:video/mp4;base64,QUJD",
		"data:image/png;base64,!!!", "data:image/png;base64,",
		"data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(8<<20)+128),
	} {
		_, _, err := decodeGrokTestMedia(invalid, "image/")
		require.Error(t, err)
	}
	_, mimeType, err := decodeGrokTestMedia("data:image/png;base64,QUJD", "image/")
	require.NoError(t, err)
	require.Equal(t, "image/png", mimeType)
}
