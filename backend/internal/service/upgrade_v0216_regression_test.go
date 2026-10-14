package service

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type interruptedV216Reader struct{ sent bool }

func (r *interruptedV216Reader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("interrupted")
	}
	r.sent = true
	return copy(p, `{"error":{"message":"unsupported"}}`), nil
}

func TestV216CompatibilityErrorCompleteness(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"unsupported","type":"invalid_request_error"}}`,
		`{"error":{"message":"unsupported","code":null}}`,
	} {
		_, complete := readOpenAICompatibilityErrorBody(strings.NewReader(body))
		require.True(t, complete)
	}
	for _, body := range []string{
		`{"error":{"message":"unsupported","message":"accepted"}}`,
		`{"error":{"message":42}}`, `{"error":"unsupported"}`, `{"error":{}}`,
		`{"error":{"message":"unsupported"}`, strings.Repeat("x", (2<<20)+1),
	} {
		_, complete := readOpenAICompatibilityErrorBody(strings.NewReader(body))
		require.False(t, complete, body[:min(len(body), 80)])
	}
	_, complete := readOpenAICompatibilityErrorBody(&interruptedV216Reader{})
	require.False(t, complete)
	_, complete = readOpenAICompatibilityErrorBody(io.NopCloser(bytes.NewReader(nil)))
	require.False(t, complete)
}

func TestV216HaikuCapabilityAndSampling(t *testing.T) {
	for _, model := range []string{"claude-haiku-5-5", "anthropic/claude-haiku-5-5-thinking", "global.anthropic.claude-haiku-5-5-20261007-v1:0", "claude-haiku-5-5@20261007"} {
		require.True(t, claude.IsHaiku55(model), model)
	}
	require.False(t, claude.IsHaiku55("claude-haiku-5-5-other"))
	for _, body := range []string{`{}`, `{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, `{"top_p":0.99,"tool_choice":{"type":"tool","name":"test"}}`} {
		_, err := normalizeOpus55Request([]byte(body), "claude-haiku-5-5")
		require.NoError(t, err)
	}
	for _, body := range []string{`{"thinking":{"type":"enabled"}}`, `{"thinking":{"type":"between_tools"}}`, `{"thinking":{"type":"disabled"},"output_config":{"effort":"max"}}`, `{"temperature":1,"top_p":0.99}`, `{"top_p":1}`, `{"top_k":0}`} {
		_, err := normalizeOpus55Request([]byte(body), "claude-haiku-5-5")
		require.Error(t, err, body)
	}
}

func TestV216GeminiOfficialEndpointAndCacheError(t *testing.T) {
	for _, endpoint := range []string{"", "https://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com:443/"} {
		require.True(t, isOfficialGeminiAPIBaseURL(endpoint))
	}
	for _, endpoint := range []string{"http://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com.evil/", "https://user@generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com?", "https://generativelanguage.googleapis.com/proxy", "https://generativelanguage.googleapis.com:444"} {
		require.False(t, isOfficialGeminiAPIBaseURL(endpoint), endpoint)
	}
	valid := []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"CachedContent not found (or permission denied)"}}`)
	require.True(t, isGeminiCachedContentNotFound(403, valid))
	require.False(t, isGeminiCachedContentNotFound(401, valid))
	require.False(t, isGeminiCachedContentNotFound(403, []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"API key invalid; cachedcontent not found in logs"}}`)))
}

func TestV216VertexAPIKeyAuthAndExpress(t *testing.T) {
	account := &Account{Platform: PlatformGemini, Type: AccountTypeServiceAccount, Credentials: map[string]any{"auth_mode": "apikey", "api_key": "AQ.Ab-test"}}
	url, err := buildVertexGeminiAccountURL(account, "gemini-3.6-flash", "streamGenerateContent", true)
	require.NoError(t, err)
	require.Equal(t, "https://aiplatform.googleapis.com/v1/publishers/google/models/gemini-3.6-flash:streamGenerateContent?alt=sse", url)
	req, _ := http.NewRequest("POST", url, nil)
	req.Header.Set("Authorization", "Bearer stale")
	require.NoError(t, setVertexGeminiAuth(t.Context(), req, account, nil))
	require.Empty(t, req.Header.Get("Authorization"))
	require.Equal(t, "AQ.Ab-test", req.Header.Get("x-goog-api-key"))
	account.Platform = PlatformAnthropic
	require.False(t, account.IsVertexAPIKey())
}

func TestV216ManifestContextPreservesExplicitValue(t *testing.T) {
	result := convertOpenAIModelListToCodexManifest([]byte(`{"data":[{"id":"gpt-6.1-sol"},{"id":"gpt-6.1-sol-2026-10-01","context_window":123},{"id":"gpt-5.6-sol"}]}`))
	require.Equal(t, int64(500000), gjson.GetBytes(result, "models.0.context_window").Int())
	require.Equal(t, int64(123), gjson.GetBytes(result, "models.1.context_window").Int())
	require.False(t, gjson.GetBytes(result, "models.2.context_window").Exists())
}
