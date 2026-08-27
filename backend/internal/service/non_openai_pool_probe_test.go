package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

type nonOpenAIPoolProbeUpstream struct {
	request *http.Request
	body    string
	status  int
}

type nonOpenAIPoolProbeAccountRepo struct {
	AccountRepository
	account *Account
}

func (r *nonOpenAIPoolProbeAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (u *nonOpenAIPoolProbeUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (u *nonOpenAIPoolProbeUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.request = req
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(u.body))}, nil
}

func nonOpenAIPoolProbeTestService(upstream HTTPUpstream) *AccountTestService {
	return &AccountTestService{httpUpstream: upstream, cfg: &config.Config{}}
}

func TestNonOpenAIPoolProbeCNProviderUsesConfiguredProtocol(t *testing.T) {
	tests := []struct {
		name         string
		platform     string
		protocol     string
		baseURL      string
		expectedPath string
		expectedAuth string
	}{
		{name: "chat completions", platform: PlatformKimi, protocol: APIProtocolChatCompletions, baseURL: "https://kimi.example/v1", expectedPath: "/v1/chat/completions", expectedAuth: "Bearer secret"},
		{name: "responses", platform: PlatformDeepSeek, protocol: APIProtocolResponses, baseURL: "https://deepseek.example/v1", expectedPath: "/v1/responses", expectedAuth: "Bearer secret"},
		{name: "anthropic", platform: PlatformZhipu, protocol: APIProtocolAnthropic, baseURL: "https://zhipu.example", expectedPath: "/v1/messages"},
		{name: "adaptive", platform: PlatformKimi, protocol: APIProtocolAdaptive, baseURL: "https://kimi.example/anthropic", expectedPath: "/anthropic/v1/messages"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responseBody := `{"choices":[{"message":{"content":"ok"}}]}`
			if test.protocol == APIProtocolResponses {
				responseBody = `{"output":[{"type":"message"}]}`
			} else if test.protocol == APIProtocolAnthropic || test.protocol == APIProtocolAdaptive {
				responseBody = `{"content":[{"type":"text","text":"ok"}]}`
			}
			upstream := &nonOpenAIPoolProbeUpstream{body: responseBody}
			account := &Account{
				ID: 1, Platform: test.platform, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "secret", "base_url": test.baseURL},
				Extra: map[string]any{
					cnAPIProtocolExtraKey: test.protocol,
					cnAPIBaseURLsExtraKey: map[string]any{test.protocol: test.baseURL, APIProtocolAnthropic: test.baseURL},
				},
			}
			resp, err := nonOpenAIPoolProbeTestService(upstream).probeCNProviderOnce(context.Background(), account, "probe-model")
			if err != nil {
				t.Fatalf("probe request: %v", err)
			}
			_ = resp.Body.Close()
			if got := upstream.request.URL.Path; got != test.expectedPath {
				t.Fatalf("path = %q, want %q", got, test.expectedPath)
			}
			if test.expectedAuth != "" && upstream.request.Header.Get("Authorization") != test.expectedAuth {
				t.Fatalf("authorization = %q", upstream.request.Header.Get("Authorization"))
			}
			if (test.protocol == APIProtocolAnthropic || test.protocol == APIProtocolAdaptive) && upstream.request.Header.Get("x-api-key") != "secret" {
				t.Fatalf("x-api-key = %q", upstream.request.Header.Get("x-api-key"))
			}
		})
	}
}

func TestNonOpenAIPoolProbeRejectsTwoHundredErrorBody(t *testing.T) {
	account := &Account{Platform: PlatformDeepSeek, Type: AccountTypeAPIKey, Extra: map[string]any{cnAPIProtocolExtraKey: APIProtocolChatCompletions}}
	if nonOpenAIPoolProbeResponseValid(account, NonOpenAIPoolRequestKindText, "deepseek-chat", []byte(`{"error":{"message":"failed"}}`)) {
		t.Fatal("200 error response must not recover the account")
	}
	if nonOpenAIPoolProbeResponseValid(account, NonOpenAIPoolRequestKindText, "deepseek-chat", []byte(`{"id":"chat_1","status":"failed"}`)) {
		t.Fatal("200 failed status must not recover the account")
	}
	if nonOpenAIPoolProbeResponseValid(account, NonOpenAIPoolRequestKindText, "deepseek-chat", []byte(`{"id":"chat_1"}`)) {
		t.Fatal("200 response without chat choices must not recover the account")
	}
}

func TestNonOpenAIPoolProbeValidatesSSEFrames(t *testing.T) {
	gemini := &Account{Platform: PlatformGemini}
	if !nonOpenAIPoolProbeResponseValid(gemini, NonOpenAIPoolRequestKindText, "gemini-2.0-flash", []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\ndata: [DONE]\n")) {
		t.Fatal("valid Gemini SSE response should recover the account")
	}
	if !nonOpenAIPoolProbeResponseValid(gemini, NonOpenAIPoolRequestKindText, "gemini-2.0-flash", []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}}`)) {
		t.Fatal("valid Gemini Code Assist wrapper should recover the account")
	}
	for _, body := range []string{
		"data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n",
		"data: {\"candidates\":[{}]}\n",
		"data: {\"candidates\":[{\"finishReason\":\"SAFETY\"}]}\n",
		"data: {\"candidates\":[{}]}\n\ndata: {\"error\":{\"message\":\"failed\"}}\n",
		"data: [DONE]\n",
	} {
		if nonOpenAIPoolProbeResponseValid(gemini, NonOpenAIPoolRequestKindText, "gemini-2.0-flash", []byte(body)) {
			t.Fatalf("invalid Gemini SSE response recovered account: %s", body)
		}
	}
	antigravity := &Account{Platform: PlatformAntigravity}
	if nonOpenAIPoolProbeResponseValid(antigravity, NonOpenAIPoolRequestKindText, "claude-sonnet-4-5", []byte(`{"response":{"error":{"message":"failed"}}}`)) {
		t.Fatal("nested Antigravity error must not recover the account")
	}
	if nonOpenAIPoolProbeResponseValid(antigravity, NonOpenAIPoolRequestKindText, "gemini-2.5-flash", []byte(`{"response":{"candidates":[{"finishReason":"SAFETY"}]}}`)) {
		t.Fatal("nested Antigravity safety block must not recover the account")
	}
}

func TestNonOpenAIPoolProbeAllowsMetadataAfterValidSSEContent(t *testing.T) {
	account := &Account{Platform: PlatformGemini}
	body := []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n" +
		"data: {\"usageMetadata\":{\"candidatesTokenCount\":1}}\n\n" +
		"data: [DONE]\n")
	if !nonOpenAIPoolProbeResponseValid(account, NonOpenAIPoolRequestKindText, "gemini-2.0-flash", body) {
		t.Fatal("metadata-only terminal frame should not invalidate valid Gemini content")
	}
}

func TestNonOpenAIPoolProbeRejectsMetadataOnlySSE(t *testing.T) {
	account := &Account{Platform: PlatformGemini}
	body := []byte("data: {\"usageMetadata\":{\"candidatesTokenCount\":1}}\n\ndata: [DONE]\n")
	if nonOpenAIPoolProbeResponseValid(account, NonOpenAIPoolRequestKindText, "gemini-2.0-flash", body) {
		t.Fatal("metadata-only Gemini stream must not recover the account")
	}
}

func TestNonOpenAIPoolProbeGrokMediaSelectsEndpointFromModel(t *testing.T) {
	tests := []struct {
		model         string
		path          string
		expectedModel string
	}{
		{model: "grok-imagine-image", path: "/v1/images/generations", expectedModel: "grok-imagine-image"},
		{model: "grok-imagine-video-1.5", path: "/v1/videos/generations", expectedModel: "grok-imagine-video"},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			upstream := &nonOpenAIPoolProbeUpstream{body: `{"data":[{"url":"https://example.com/result"}]}`}
			account := &Account{ID: 2, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "secret", "base_url": "https://api.x.ai"}}
			resp, err := nonOpenAIPoolProbeTestService(upstream).probeGrokOnce(context.Background(), account, NonOpenAIPoolRequestKindImage, test.model)
			if err != nil {
				t.Fatalf("probe request: %v", err)
			}
			_ = resp.Body.Close()
			if got := upstream.request.URL.Path; got != test.path {
				t.Fatalf("path = %q, want %q", got, test.path)
			}
			if upstream.request.Header.Get("Authorization") != "Bearer secret" {
				t.Fatalf("authorization = %q", upstream.request.Header.Get("Authorization"))
			}
			var payload map[string]any
			if err := json.NewDecoder(upstream.request.Body).Decode(&payload); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if payload["model"] != test.expectedModel {
				t.Fatalf("model = %v, want %q", payload["model"], test.expectedModel)
			}
			if strings.Contains(test.model, "video") {
				if _, exists := payload["duration"]; exists {
					t.Fatal("video recovery probe must not send an unsupported one-second duration")
				}
			}
		})
	}
}

func TestNonOpenAIPoolProbeGrokTextUsesResponses(t *testing.T) {
	upstream := &nonOpenAIPoolProbeUpstream{body: `{"status":"completed","output":[]}`}
	account := &Account{ID: 21, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "secret", "base_url": "https://api.x.ai"}}
	resp, err := nonOpenAIPoolProbeTestService(upstream).probeGrokOnce(context.Background(), account, NonOpenAIPoolRequestKindText, "grok-4.5")
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	_ = resp.Body.Close()
	if got := upstream.request.URL.Path; got != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", got)
	}
	if upstream.request.Header.Get("Authorization") != "Bearer secret" {
		t.Fatalf("authorization = %q", upstream.request.Header.Get("Authorization"))
	}
}

func TestNonOpenAIPoolProbeAntigravityAPIKeyUsesNativeProtocol(t *testing.T) {
	tests := []struct {
		model      string
		mapped     string
		pathSuffix string
		authHeader string
	}{
		{model: "claude-probe", mapped: "claude-sonnet-4-5", pathSuffix: "/antigravity/v1/messages", authHeader: "x-api-key"},
		{model: "custom-image-probe", mapped: "gemini-2.5-flash", pathSuffix: "/v1beta/models/gemini-2.5-flash:streamGenerateContent", authHeader: "x-goog-api-key"},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			upstream := &nonOpenAIPoolProbeUpstream{body: `{"content":[{"type":"text","text":"ok"}]}`}
			account := &Account{
				ID: 22, Platform: PlatformAntigravity, Type: AccountTypeAPIKey,
				Credentials: map[string]any{
					"api_key": "secret", "base_url": "https://antigravity.example",
					"model_mapping": map[string]any{test.model: test.mapped},
				},
			}
			resp, err := nonOpenAIPoolProbeTestService(upstream).probeAntigravityOnce(context.Background(), account, test.model)
			if err != nil {
				t.Fatalf("probe request: %v", err)
			}
			_ = resp.Body.Close()
			if got := upstream.request.URL.Path; got != test.pathSuffix {
				t.Fatalf("path = %q, want %q", got, test.pathSuffix)
			}
			if upstream.request.Header.Get(test.authHeader) != "secret" {
				t.Fatalf("%s = %q", test.authHeader, upstream.request.Header.Get(test.authHeader))
			}
		})
	}
}

func TestNonOpenAIPoolProbeValidatesProtocolSuccessShape(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		body     string
		expected bool
	}{
		{name: "chat", account: &Account{Platform: PlatformKimi, Extra: map[string]any{cnAPIProtocolExtraKey: APIProtocolChatCompletions}}, body: `{"choices":[{"message":{"content":"ok"}}]}`, expected: true},
		{name: "responses empty output", account: &Account{Platform: PlatformDeepSeek, Extra: map[string]any{cnAPIProtocolExtraKey: APIProtocolResponses}}, body: `{"id":"resp_1","status":"completed","output":[]}`, expected: true},
		{name: "anthropic", account: &Account{Platform: PlatformZhipu, Extra: map[string]any{cnAPIProtocolExtraKey: APIProtocolAnthropic}}, body: `{"content":[{"type":"text","text":"ok"}]}`, expected: true},
		{name: "antigravity native anthropic", account: &Account{Platform: PlatformAntigravity}, body: `{"content":[{"type":"text","text":"ok"}]}`, expected: true},
		{name: "responses incomplete", account: &Account{Platform: PlatformDeepSeek, Extra: map[string]any{cnAPIProtocolExtraKey: APIProtocolResponses}}, body: `{"id":"resp_1","status":"incomplete","output":[]}`, expected: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nonOpenAIPoolProbeResponseValid(test.account, NonOpenAIPoolRequestKindText, "probe-model", []byte(test.body)); got != test.expected {
				t.Fatalf("valid = %v, want %v", got, test.expected)
			}
		})
	}
}

func TestNonOpenAIPoolProbeGeminiAPIKeyUsesMappedModel(t *testing.T) {
	upstream := &nonOpenAIPoolProbeUpstream{body: `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`}
	account := &Account{
		ID: 3, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"api_key": "secret", "base_url": "https://generativelanguage.googleapis.com",
			"model_mapping": map[string]any{"probe-alias": "gemini-2.0-flash"},
			"pool_mode":     true,
		},
	}
	repo := &nonOpenAIPoolProbeAccountRepo{account: account}
	service := nonOpenAIPoolProbeTestService(upstream)
	service.accountRepo = repo
	result := service.runNonOpenAIPoolProbe(context.Background(), account.ID, PlatformGemini, NonOpenAIPoolRequestKindText, "probe-alias")
	if !result.Success {
		t.Fatalf("probe result = %+v", result)
	}
	if !strings.Contains(upstream.request.URL.Path, "/models/gemini-2.0-flash:streamGenerateContent") {
		t.Fatalf("mapped model path = %q", upstream.request.URL.Path)
	}
	if upstream.request.Header.Get("x-goog-api-key") != "secret" {
		t.Fatalf("x-goog-api-key = %q", upstream.request.Header.Get("x-goog-api-key"))
	}
}

func TestNonOpenAIPoolProbeSkipsDisabledOrUnschedulableAccount(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      string
		schedulable bool
	}{
		{name: "disabled", status: StatusDisabled, schedulable: true},
		{name: "unschedulable", status: StatusActive, schedulable: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &nonOpenAIPoolProbeUpstream{body: `{"candidates":[{}]}`}
			account := &Account{
				ID: 31, Platform: PlatformGemini, Type: AccountTypeAPIKey,
				Status: test.status, Schedulable: test.schedulable,
				Credentials: map[string]any{"api_key": "secret", "pool_mode": true},
			}
			service := nonOpenAIPoolProbeTestService(upstream)
			service.accountRepo = &nonOpenAIPoolProbeAccountRepo{account: account}
			result := service.runNonOpenAIPoolProbe(context.Background(), account.ID, account.Platform, NonOpenAIPoolRequestKindText, "gemini-2.0-flash")
			if !result.Success || result.Source != "account_unschedulable" {
				t.Fatalf("probe result = %+v", result)
			}
			if upstream.request != nil {
				t.Fatal("disabled or unschedulable account must not receive an upstream probe")
			}
		})
	}
}
