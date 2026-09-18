package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGrokPoolContentRejectionKeepsAccountsAvailableAcrossProtocols(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "bridge", "media"} {
		t.Run(protocol, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusForbidden, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(`{"error":{"code":"content_policy_violation","message":"prompt violates policy"}}`)),
			}}
			runtime := NewNonOpenAIPoolRuntime()
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, nonOpenAIPoolRuntime: runtime}
			account := nonOpenAIPoolTestAccount(900, PlatformGrok)
			account.Credentials["api_key"] = "test-key"
			account.Credentials["access_token"] = "test-token"
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+protocol, nil)
			var err error
			switch protocol {
			case "responses":
				_, err = svc.Forward(context.Background(), c, account, []byte(`{"model":"grok-4.6","input":"hi","stream":false}`))
			case "chat":
				_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"stream":false}`), "", "")
			case "bridge":
				account.Type = AccountTypeOAuth
				_, err = svc.forwardGrokChatCompletionsViaResponses(context.Background(), c, account, []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}],"stream":false}`), "", "")
			case "media":
				_, err = svc.handleGrokMediaErrorResponse(context.Background(), upstream.resp, c, account, "")
			}
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover), "a rejected prompt must not rotate accounts")
			require.False(t, runtime.stateForAccount(account).Cooling)
		})
	}
}

func TestGrokNativeSearchMappingIdentityAndSuccessValidation(t *testing.T) {
	for _, tc := range []struct {
		name, response, base string
		success              bool
	}{
		{"valid", `{"status":"completed","error":null,"output":[]}`, "", true},
		{"custom", `{"status":"completed","output":[]}`, "https://relay.example/v1", true},
		{"error", `{"error":{"message":"private-provider secret"}}`, "", false},
		{"html", "<html>private-provider secret</html>", "", false},
		{"pending", `{"status":"in_progress","output":[]}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.response))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 901, Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{
				"access_token": "test-token", "base_url": tc.base, "model_mapping": map[string]any{"grok-4.5": "grok-4.6"},
			}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/web_search", nil)
			result, err := svc.ForwardGrokWebSearch(context.Background(), c, account, []byte(`{"query":"hi"}`))
			require.Equal(t, "grok-4.6", gjson.GetBytes(upstream.lastBody, "model").String())
			if tc.base == "" {
				require.Equal(t, "grok-shell", upstream.lastReq.Header.Get("X-Grok-Client-Identifier"))
			} else {
				require.Empty(t, upstream.lastReq.Header.Get("X-Grok-Client-Identifier"))
			}
			if tc.success {
				require.NoError(t, err)
				require.NotNil(t, result)
			} else {
				require.Error(t, err)
				require.Nil(t, result)
				require.NotContains(t, err.Error(), "private-provider")
			}
		})
	}
}

func TestGrokTypedMappingMatchesPersistedMapping(t *testing.T) {
	for _, raw := range []any{map[string]string{"my-chat": "grok-4.6"}, map[string]any{"my-chat": "grok-4.6"}} {
		account := &Account{Platform: PlatformGrok, Credentials: map[string]any{"model_mapping": raw}}
		require.True(t, account.IsModelSupported("my-chat"))
		require.False(t, account.IsModelSupported("other-model"))
		require.Equal(t, "grok-4.6", account.GetMappedModel("my-chat"))
	}
}

func TestGrokVoiceRejectsNominalSuccessErrors(t *testing.T) {
	for _, endpoint := range []string{"tts", "stt", "custom-voices"} {
		for _, body := range []string{`{"error":{"message":"failed"}}`, "<html>error</html>", `{"status":"incomplete"}`} {
			require.False(t, grokVoiceResponseValid(endpoint, "application/json", []byte(body)))
		}
	}
	require.True(t, grokVoiceResponseValid("tts", "audio/mpeg", []byte("audio")))
	require.False(t, grokVoiceResponseValid("tts", "audio/mpeg", nil))
	require.True(t, grokVoiceResponseValid("stt", "application/json", []byte(`{"text":"","error":null}`)))
}

func TestGrokVoiceForwardingValidatesBeforeCommitting(t *testing.T) {
	for _, tc := range []struct {
		endpoint, body, contentType string
		status                      int
		success                     bool
	}{
		{"tts", "audio", "audio/mpeg", 200, true},
		{"stt", `{"text":"hi","error":null}`, "application/json", 200, true},
		{"tts", `{"error":{"message":"private-provider"}}`, "application/json", 200, false},
		{"stt", "<html>private-provider</html>", "text/html", 200, false},
		{"custom-voices/id", "", "", 204, true},
	} {
		t.Run(tc.endpoint+tc.contentType, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 902, Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "test-token"}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+tc.endpoint, nil)
			result, err := svc.ForwardGrokVoice(context.Background(), c, account, tc.endpoint, []byte(`{"text":"hi"}`), "application/json")
			require.Equal(t, "grok-shell", upstream.lastReq.Header.Get("X-Grok-Client-Identifier"))
			if tc.success {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, tc.status, recorder.Code)
			} else {
				require.Error(t, err)
				require.Nil(t, result)
				require.False(t, c.Writer.Written())
				require.NotContains(t, err.Error(), "private-provider")
			}
		})
	}
}

func TestGrokRecoveryProbeRecognizesItsOwnOutputLimit(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		success    bool
	}{
		{"completed", `{"status":"completed","output":[],"error":null}`, true},
		{"one_token", `{"status":"incomplete","output":[],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"output_tokens":1}}`, true},
		{"no_usage", `{"status":"incomplete","output":[],"incomplete_details":{"reason":"max_output_tokens"}}`, false},
		{"filtered", `{"status":"incomplete","output":[],"incomplete_details":{"reason":"content_filter"},"usage":{"output_tokens":1}}`, false},
		{"pending", `{"status":"in_progress","output":[]}`, false},
		{"error", `{"status":"completed","output":[],"error":{"message":"failed"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := nonOpenAIPoolTestAccount(903, PlatformGrok)
			account.Status, account.Schedulable = StatusActive, true
			account.Credentials["api_key"] = "test-key"
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}}
			svc := &AccountTestService{accountRepo: &nonOpenAIPoolProbeAccountRepo{account: account}, httpUpstream: upstream, cfg: &config.Config{}}
			result := svc.runNonOpenAIPoolProbe(context.Background(), account.ID, PlatformGrok, NonOpenAIPoolRequestKindText, "grok-4.6")
			require.Equal(t, tc.success, result.Success)
			require.Equal(t, int64(1), gjson.GetBytes(upstream.lastBody, "max_output_tokens").Int())
		})
	}
}
