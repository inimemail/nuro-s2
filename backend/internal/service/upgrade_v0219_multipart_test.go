package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpgradeV0219GrokMalformedMultipartRejected(t *testing.T) {
	const contentType = "multipart/form-data; boundary=review-boundary"
	const prefix = "--review-boundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ngrok-imagine-image\r\n" +
		"--review-boundary\r\nContent-Disposition: form-data; name=\"image\"; filename=\"test.png\"\r\nContent-Type: image/png\r\n\r\nfixture-image\r\n"
	cases := []struct {
		name, contentType, body string
	}{
		{"missing_boundary", "multipart/form-data", prefix},
		{"invalid_content_type", `multipart/form-data; boundary="unterminated`, prefix},
		{"invalid_next_headers", contentType, prefix + "--review-boundary\r\nNot-A-Header\r\n\r\nignored\r\n--review-boundary--\r\n"},
		{"truncated_next_headers", contentType, prefix + "--review-boundary\r\nContent-Disposition: form-data; name=\"image\""},
		{"truncated_upload", contentType, prefix},
		{"closing_marker_in_unterminated_headers", contentType, prefix + "--review-boundary\r\n--review-boundary--"},
		{"closing_marker_in_header_value", contentType, prefix + "--review-boundary\r\nX-Note: --review-boundary--"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			t.Run("metadata", func(t *testing.T) {
				info := ParseGrokMediaRequest(tc.contentType, body)
				require.Error(t, info.ParseError, "a partially parsed upload must not be accepted")
			})
			t.Run("conversion", func(t *testing.T) {
				converted, _, err := prepareGrokMediaForwardBody(GrokMediaEndpointImagesEdits, body, tc.contentType)
				require.Error(t, err, "malformed uploads must not become partial JSON requests")
				require.Empty(t, converted)
			})
			t.Run("openai_images", func(t *testing.T) {
				openAIBody := []byte(strings.Replace(tc.body, "grok-imagine-image", "gpt-image-1", 1))
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(string(openAIBody)))
				c.Request.Header.Set("Content-Type", tc.contentType)
				parsed, err := (&OpenAIGatewayService{}).ParseOpenAIImagesRequest(c, openAIBody)
				require.Error(t, err, "OpenAI image uploads must reject truncated MIME headers too")
				require.Nil(t, parsed)
			})
			t.Run("forward", func(t *testing.T) {
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
				}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{ID: 71, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-key"}}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(tc.body))
				result, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointImagesEdits, "", body, tc.contentType)
				require.Error(t, err)
				require.Nil(t, result)
				require.Nil(t, upstream.lastReq, "invalid input must not issue an upstream request")
			})
		})
	}

	valid := prefix + "--review-boundary--\r\n"
	for name, raw := range map[string]string{
		"crlf":                      valid,
		"no_final_newline":          strings.TrimSuffix(valid, "\r\n"),
		"lf":                        strings.ReplaceAll(valid, "\r\n", "\n"),
		"lf_no_final_newline":       strings.TrimSuffix(strings.ReplaceAll(valid, "\r\n", "\n"), "\n"),
		"preamble_and_epilogue":     "ignored preamble\n" + valid + "ignored epilogue\n",
		"boundary_whitespace":       strings.TrimSuffix(valid, "\r\n") + " \t\r\n",
		"first_boundary_whitespace": strings.Replace(valid, "--review-boundary\r\n", "--review-boundary \t\r\n", 1),
	} {
		t.Run("valid_multipart_unchanged/"+name, func(t *testing.T) {
			body := []byte(raw)
			info := ParseGrokMediaRequest(contentType, body)
			require.NoError(t, info.ParseError)
			require.Equal(t, "grok-imagine-image", info.Model)
			require.Len(t, info.Uploads, 1)
			require.Equal(t, "fixture-image", string(info.Uploads[0].Data))
			converted, convertedType, err := prepareGrokMediaForwardBody(GrokMediaEndpointImagesEdits, body, contentType)
			require.NoError(t, err)
			require.Equal(t, "application/json", convertedType)
			require.Contains(t, string(converted), "Zml4dHVyZS1pbWFnZQ==")
			openAIBody := []byte(strings.Replace(raw, "grok-imagine-image", "gpt-image-1", 1))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(string(openAIBody)))
			c.Request.Header.Set("Content-Type", contentType)
			parsed, err := (&OpenAIGatewayService{}).ParseOpenAIImagesRequest(c, openAIBody)
			require.NoError(t, err)
			require.Equal(t, "gpt-image-1", parsed.Model)
			require.Len(t, parsed.Uploads, 1)
			require.Equal(t, "fixture-image", string(parsed.Uploads[0].Data))
		})
	}
	t.Run("bodyless_lookup_unchanged", func(t *testing.T) {
		for _, header := range []string{contentType, "multipart/form-data", `multipart/form-data; boundary="unterminated`} {
			require.Equal(t, ParseGrokMediaRequest("", nil), ParseGrokMediaRequest(header, nil))
		}
	})
	t.Run("json_unchanged", func(t *testing.T) {
		body := []byte(`{"model":"grok-imagine-image","image":{"url":"https://example.com/image.png"}}`)
		info := ParseGrokMediaRequest("application/json", body)
		require.NoError(t, info.ParseError)
		require.Equal(t, "grok-imagine-image", info.Model)
		converted, convertedType, err := prepareGrokMediaForwardBody(GrokMediaEndpointImagesEdits, body, "application/json")
		require.NoError(t, err)
		require.Equal(t, "application/json", convertedType)
		require.Equal(t, body, converted)
	})
}
