package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

type codexModelsMemoryUpstream struct {
	do func(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error)
}

func (u *codexModelsMemoryUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.do(req, proxyURL, accountID, accountConcurrency)
}

func (u *codexModelsMemoryUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func newCodexModelsAPIKeyTestService(upstream HTTPUpstream) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled: false,
		}}},
		httpUpstream: upstream,
	}
}

func newCodexModelsAPIKeyTestAccount(baseURL string) *Account {
	return &Account{
		ID:          2,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 3,
		Credentials: map[string]any{
			"api_key":  "sk-upstream",
			"base_url": baseURL,
		},
	}
}

func TestFetchCodexModelsManifestCustomAPIKeyRequiresRuntimeConfig(t *testing.T) {
	svc := &OpenAIGatewayService{}
	manifest, err := svc.FetchCodexModelsManifest(
		context.Background(),
		newCodexModelsAPIKeyTestAccount("https://upstream.example"),
		"0.144.0",
		"",
	)
	if err == nil || manifest != nil {
		t.Fatalf("missing config returned manifest=%#v err=%v", manifest, err)
	}
}

func newCodexModelsTestAccount() *Account {
	return &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "test-access-token",
			"chatgpt_account_id": "acc-123",
		},
	}
}

func TestFetchCodexModelsManifestPassthrough(t *testing.T) {
	manifestBody := `{"models":[{"slug":"gpt-5.5","display_name":"GPT-5.5"}]}`

	var gotAuth, gotAccountID, gotOriginator, gotClientVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccountID = r.Header.Get("chatgpt-account-id")
		gotOriginator = r.Header.Get("Originator")
		gotClientVersion = r.URL.Query().Get("client_version")
		w.Header().Set("ETag", `W/"abc123"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(manifestBody))
	}))
	defer server.Close()

	original := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = original }()

	s := &OpenAIGatewayService{}
	manifest, err := s.FetchCodexModelsManifest(context.Background(), newCodexModelsTestAccount(), "0.137.0", "")
	if err != nil {
		t.Fatalf("FetchCodexModelsManifest returned error: %v", err)
	}

	if string(manifest.Body) != manifestBody {
		t.Errorf("body not passed through verbatim: got %q", manifest.Body)
	}
	if manifest.ETag != `W/"abc123"` {
		t.Errorf("etag not passed through: got %q", manifest.ETag)
	}
	if gotAuth != "Bearer test-access-token" {
		t.Errorf("authorization header: got %q", gotAuth)
	}
	if gotAccountID != "acc-123" {
		t.Errorf("chatgpt-account-id header: got %q", gotAccountID)
	}
	if gotOriginator != "codex_cli_rs" {
		t.Errorf("originator header: got %q", gotOriginator)
	}
	if gotClientVersion != "0.137.0" {
		t.Errorf("client_version query: got %q", gotClientVersion)
	}
}

func TestFetchCodexModelsManifestDefaultClientVersion(t *testing.T) {
	var gotClientVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClientVersion = r.URL.Query().Get("client_version")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()

	original := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = original }()

	s := &OpenAIGatewayService{}
	if _, err := s.FetchCodexModelsManifest(context.Background(), newCodexModelsTestAccount(), "", ""); err != nil {
		t.Fatalf("FetchCodexModelsManifest returned error: %v", err)
	}
	if gotClientVersion != openAICodexProbeVersion {
		t.Errorf("default client_version: got %q, want %q", gotClientVersion, openAICodexProbeVersion)
	}
}

func TestFetchCodexModelsManifestNotModified(t *testing.T) {
	var gotIfNoneMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `W/"abc123"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	original := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = original }()

	s := &OpenAIGatewayService{}
	manifest, err := s.FetchCodexModelsManifest(context.Background(), newCodexModelsTestAccount(), "0.137.0", `W/"abc123"`)
	if err != nil {
		t.Fatalf("FetchCodexModelsManifest returned error: %v", err)
	}
	if !manifest.NotModified {
		t.Error("expected NotModified to be true")
	}
	if gotIfNoneMatch != `W/"abc123"` {
		t.Errorf("if-none-match header: got %q", gotIfNoneMatch)
	}
}

func TestFetchCodexModelsManifestUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"boom"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	original := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = original }()

	s := &OpenAIGatewayService{}
	if _, err := s.FetchCodexModelsManifest(context.Background(), newCodexModelsTestAccount(), "0.137.0", ""); err == nil {
		t.Fatal("expected error for upstream 500, got nil")
	}
}

func TestFetchCodexModelsManifestRejectsHTMLSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><title>private.example | 502</title>`))
	}))
	defer server.Close()

	original := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = original }()

	s := &OpenAIGatewayService{}
	manifest, err := s.FetchCodexModelsManifest(context.Background(), newCodexModelsTestAccount(), "0.137.0", "")
	if err == nil || manifest != nil {
		t.Fatalf("expected invalid response error, got manifest=%#v err=%v", manifest, err)
	}
}

func TestValidateCodexModelsManifestEnvelope(t *testing.T) {
	for _, valid := range []string{
		`{"models":[]}`,
		`{"models":[{"slug":"gpt-5.6","future_field":{"nested":true}}],"future_top_level":1}`,
	} {
		if err := validateCodexModelsManifestEnvelope([]byte(valid)); err != nil {
			t.Fatalf("expected valid manifest %s: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		`null`,
		`[]`,
		`{}`,
		`{"models":null}`,
		`{"models":{}}`,
		`{"models":[}`,
	} {
		if err := validateCodexModelsManifestEnvelope([]byte(invalid)); err == nil {
			t.Fatalf("expected invalid manifest: %s", invalid)
		}
	}
}

func TestFetchCodexModelsManifestMissingToken(t *testing.T) {
	account := newCodexModelsTestAccount()
	delete(account.Credentials, "access_token")

	s := &OpenAIGatewayService{}
	if _, err := s.FetchCodexModelsManifest(context.Background(), account, "0.137.0", ""); err == nil {
		t.Fatal("expected error for missing access token, got nil")
	}
}

func TestSelectCodexModelsManifestAccountSkipsAPIKey(t *testing.T) {
	apiKey := Account{
		ID:          11,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Priority:    1,
		Credentials: map[string]any{"api_key": "sk-test"},
	}
	oauth := Account{
		ID:          12,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Priority:    2,
		Credentials: map[string]any{"access_token": "oauth-token"},
	}
	s := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{apiKey, oauth}},
	}

	got, err := s.SelectCodexModelsManifestAccount(context.Background(), nil)
	if err != nil {
		t.Fatalf("SelectCodexModelsManifestAccount returned error: %v", err)
	}
	if got == nil || got.ID != oauth.ID {
		t.Fatalf("selected account = %#v, want oauth account %d", got, oauth.ID)
	}
}

func TestFetchCodexModelsManifestAPIKeyCustomUpstreamUsesIsolatedCache(t *testing.T) {
	calls := 0
	var gotURL, gotAuthorization, gotOriginator, gotAccountID string
	var gotProfile HTTPUpstreamProfile
	upstream := &codexModelsMemoryUpstream{do: func(req *http.Request, _ string, accountID int64, concurrency int) (*http.Response, error) {
		calls++
		gotURL = req.URL.String()
		gotAuthorization = req.Header.Get("Authorization")
		gotOriginator = req.Header.Get("Originator")
		gotAccountID = req.Header.Get("ChatGPT-Account-ID")
		gotProfile = HTTPUpstreamProfileFromContext(req.Context())
		if accountID != 2 || concurrency != 3 {
			t.Fatalf("unexpected upstream identity: account=%d concurrency=%d", accountID, concurrency)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Etag": []string{`W/"manifest"`}},
			Body:       io.NopCloser(strings.NewReader(`{"models":[{"slug":"gpt-5.6"}]}`)),
		}, nil
	}}
	svc := newCodexModelsAPIKeyTestService(upstream)
	account := newCodexModelsAPIKeyTestAccount("https://upstream.example/v1")

	first, err := svc.FetchCodexModelsManifest(context.Background(), account, "0.144.0", "")
	if err != nil {
		t.Fatalf("first fetch failed: %v", err)
	}
	second, err := svc.FetchCodexModelsManifest(context.Background(), account, "0.144.0", `W/"manifest"`)
	if err != nil {
		t.Fatalf("cached fetch failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if gotURL != "https://upstream.example/v1/models?client_version=0.144.0" {
		t.Fatalf("request URL = %q", gotURL)
	}
	if gotAuthorization != "Bearer sk-upstream" || gotOriginator != "codex_cli_rs" {
		t.Fatalf("unexpected headers: authorization=%q originator=%q", gotAuthorization, gotOriginator)
	}
	if gotAccountID != "" {
		t.Fatalf("API key request leaked ChatGPT account header: %q", gotAccountID)
	}
	if gotProfile != HTTPUpstreamProfileOpenAI {
		t.Fatalf("upstream profile = %q, want %q", gotProfile, HTTPUpstreamProfileOpenAI)
	}
	if string(first.Body) != `{"models":[{"slug":"gpt-5.6"}]}` {
		t.Fatalf("first body = %q", first.Body)
	}
	if !second.NotModified || second.ETag != `W/"manifest"` {
		t.Fatalf("cached conditional result = %#v", second)
	}
}

func TestFetchCodexModelsManifestInvalidEnvelopeIsRetryableAndNotCached(t *testing.T) {
	calls := 0
	upstream := &codexModelsMemoryUpstream{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		calls++
		body := `{"object":"list","data":[]}`
		if calls > 1 {
			body = `{"models":[]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}}
	svc := newCodexModelsAPIKeyTestService(upstream)
	account := newCodexModelsAPIKeyTestAccount("https://upstream.example")

	manifest, err := svc.FetchCodexModelsManifest(context.Background(), account, "0.144.0", "")
	if err == nil || manifest != nil {
		t.Fatalf("invalid manifest returned manifest=%#v err=%v", manifest, err)
	}
	if !IsRetryableCodexModelsManifestError(err) {
		t.Fatalf("invalid 2xx manifest must be retryable: %v", err)
	}
	manifest, err = svc.FetchCodexModelsManifest(context.Background(), account, "0.144.0", "")
	if err != nil || manifest == nil {
		t.Fatalf("second fetch returned manifest=%#v err=%v", manifest, err)
	}
	if calls != 2 {
		t.Fatalf("invalid envelope was cached: calls=%d", calls)
	}
}

func TestFetchCodexModelsManifestNilUpstreamResponseIsRetryable(t *testing.T) {
	upstream := &codexModelsMemoryUpstream{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		return nil, nil
	}}
	svc := newCodexModelsAPIKeyTestService(upstream)

	manifest, err := svc.FetchCodexModelsManifest(context.Background(), newCodexModelsAPIKeyTestAccount("https://upstream.example"), "0.144.0", "")
	if err == nil || manifest != nil {
		t.Fatalf("nil upstream response returned manifest=%#v err=%v", manifest, err)
	}
	if !IsRetryableCodexModelsManifestError(err) {
		t.Fatalf("nil upstream response must be retryable: %v", err)
	}
}

func TestFetchCodexModelsManifestPermanentFourHundredIsNotRetryable(t *testing.T) {
	upstream := &codexModelsMemoryUpstream{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"private upstream detail"}}`)),
		}, nil
	}}
	svc := newCodexModelsAPIKeyTestService(upstream)
	_, err := svc.FetchCodexModelsManifest(context.Background(), newCodexModelsAPIKeyTestAccount("https://upstream.example"), "0.144.0", "")
	if err == nil {
		t.Fatal("expected upstream 400 error")
	}
	if IsRetryableCodexModelsManifestError(err) {
		t.Fatalf("permanent upstream 400 must not be retryable: %v", err)
	}
	if strings.Contains(err.Error(), "private upstream detail") || strings.Contains(err.Error(), "upstream.example") {
		t.Fatalf("error contains upstream diagnostic: %v", err)
	}
}

func TestSelectCodexModelsManifestAccountSupportsCustomAPIKey(t *testing.T) {
	account := *newCodexModelsAPIKeyTestAccount("https://upstream.example/v1")
	account.Status = StatusActive
	account.Schedulable = true
	svc := &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}}

	got, err := svc.SelectCodexModelsManifestAccount(context.Background(), nil)
	if err != nil {
		t.Fatalf("SelectCodexModelsManifestAccount returned error: %v", err)
	}
	if got == nil || got.ID != account.ID {
		t.Fatalf("selected account = %#v, want custom API key account %d", got, account.ID)
	}
}
