package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

// chatgptCodexModelsURL is the ChatGPT Codex models manifest endpoint.
// Package-level variable so tests can point it at a stub server.
var chatgptCodexModelsURL = "https://chatgpt.com/backend-api/codex/models"

const codexModelsManifestBodyLimit int64 = 8 << 20

// CodexModelsManifest carries the raw upstream manifest payload plus caching metadata.
type CodexModelsManifest struct {
	Body        []byte
	ETag        string
	NotModified bool
}

// SelectCodexModelsManifestAccount selects an OpenAI OAuth account that can
// authenticate against ChatGPT's Codex manifest endpoint. API key accounts may
// serve normal OpenAI-compatible traffic, but this endpoint requires a ChatGPT
// access token, so mixed OpenAI pools must skip API key accounts here.
func (s *OpenAIGatewayService) SelectCodexModelsManifestAccount(ctx context.Context, groupID *int64) (*Account, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_CODEX_MODELS_SERVICE_UNAVAILABLE", "OpenAI gateway service is unavailable")
	}
	excluded := make(map[int64]struct{})
	for {
		account, err := s.SelectAccountForModelWithExclusions(ctx, groupID, "", "", excluded)
		if err != nil {
			return nil, err
		}
		if account == nil || account.ID <= 0 {
			return nil, infraerrors.New(http.StatusServiceUnavailable, "OPENAI_CODEX_MODELS_ACCOUNT_UNAVAILABLE", "no OpenAI OAuth account is available")
		}
		if s.codexModelsManifestAccountUsable(ctx, account) {
			return account, nil
		}
		excluded[account.ID] = struct{}{}
	}
}

func (s *OpenAIGatewayService) codexModelsManifestAccountUsable(ctx context.Context, account *Account) bool {
	if account == nil || !account.IsOpenAIOAuth() {
		return false
	}
	credAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil || credAccount == nil || !credAccount.IsOpenAIOAuth() {
		return false
	}
	return credAccount.IsOpenAIAgentIdentity() || strings.TrimSpace(credAccount.GetOpenAIAccessToken()) != ""
}

// FetchCodexModelsManifest fetches the live Codex models manifest with OAuth credentials.
func (s *OpenAIGatewayService) FetchCodexModelsManifest(ctx context.Context, account *Account, clientVersion, ifNoneMatch string) (*CodexModelsManifest, error) {
	if account == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_CODEX_MODELS_ACCOUNT_REQUIRED", "account is required")
	}
	credAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_CODEX_MODELS_CREDENTIALS_FAILED", "resolve credential account: %v", err)
	}
	if !credAccount.IsOpenAIOAuth() {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_MODELS_ACCOUNT_TYPE_UNSUPPORTED", "Codex models manifest requires an OpenAI OAuth account")
	}
	accessToken := credAccount.GetOpenAIAccessToken()
	if accessToken == "" && !credAccount.IsOpenAIAgentIdentity() {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_MODELS_TOKEN_MISSING", "account has no Codex backend access token")
	}

	clientVersion = strings.TrimSpace(clientVersion)
	if clientVersion == "" {
		clientVersion = openAICodexProbeVersion
	}
	requestURL := chatgptCodexModelsURL + "?client_version=" + url.QueryEscape(clientVersion)

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_CODEX_MODELS_REQUEST_FAILED", "create codex models request: %v", err)
	}
	authHeaders, authErr := s.buildOpenAIAuthenticationHeaders(reqCtx, credAccount, accessToken)
	if authErr != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_MODELS_AUTH_FAILED", "failed to build Codex models authentication")
	}
	for key, values := range authHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Originator", "codex_cli_rs")
	req.Header.Set("Version", clientVersion)
	req.Header.Set("User-Agent", codexCLIUserAgent)
	if ifNoneMatch = strings.TrimSpace(ifNoneMatch); ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	setOpenAIChatGPTAccountHeaders(req.Header, credAccount)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:              proxyURL,
		Timeout:               15 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_CODEX_MODELS_PROXY_INVALID", "invalid proxy configuration: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_CODEX_MODELS_UPSTREAM_FAILED", "codex models manifest request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		return &CodexModelsManifest{ETag: resp.Header.Get("ETag"), NotModified: true}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if credAccount.IsOpenAIAgentIdentity() && !agentIdentityTaskRecoveryWasTried(ctx) && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, body) {
			expectedTaskID := credAccount.GetCredential("task_id")
			if recoveryErr := s.recoverAgentIdentityTask(ctx, credAccount, expectedTaskID); recoveryErr != nil {
				return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_MODELS_AUTH_FAILED", "Agent Identity task recovery failed")
			}
			return s.FetchCodexModelsManifest(markAgentIdentityTaskRecoveryTried(ctx), account, clientVersion, ifNoneMatch)
		}
		_ = redactAgentIdentitySensitiveBodyForAccount(ctx, s.accountRepo, credAccount, body)
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_CODEX_MODELS_UPSTREAM_FAILED", "codex models manifest upstream returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, codexModelsManifestBodyLimit))
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_CODEX_MODELS_UPSTREAM_FAILED", "read codex models manifest response: %v", err)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_MODELS_INVALID_RESPONSE", "models manifest response is not a JSON object")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_MODELS_INVALID_RESPONSE", "models manifest response is invalid")
	}
	if rawError, ok := envelope["error"]; ok && string(bytes.TrimSpace(rawError)) != "null" {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_MODELS_INVALID_RESPONSE", "models manifest response contains an error")
	}
	return &CodexModelsManifest{Body: body, ETag: resp.Header.Get("ETag")}, nil
}
