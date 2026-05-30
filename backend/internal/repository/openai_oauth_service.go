package repository

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/imroc/req/v3"
)

// NewOpenAIOAuthClient creates a new OpenAI OAuth client
func NewOpenAIOAuthClient() service.OpenAIOAuthClient {
	return &openaiOAuthService{
		tokenURL:          openai.TokenURL,
		deviceUserCodeURL: openai.DeviceUserCodeURL,
		deviceTokenURL:    openai.DeviceTokenURL,
	}
}

type openaiOAuthService struct {
	tokenURL          string
	deviceUserCodeURL string
	deviceTokenURL    string
}

func (s *openaiOAuthService) StartDeviceAuth(ctx context.Context, proxyURL string) (*openai.DeviceAuthStartResponse, error) {
	client, err := createOpenAIReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_DEVICE_AUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	payload := map[string]string{"client_id": openai.ClientID}

	var startResp openai.DeviceAuthStartResponse
	resp, err := client.R().
		SetContext(ctx).
		SetHeader("User-Agent", "codex-cli/0.91.0").
		SetHeader("Content-Type", "application/json").
		SetBody(payload).
		SetSuccessResult(&startResp).
		Post(s.deviceUserCodeURL)

	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_DEVICE_AUTH_REQUEST_FAILED", "request failed: %v", err)
	}
	if !resp.IsSuccessState() {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_DEVICE_AUTH_START_FAILED", "device auth start failed: status %d, body: %s", resp.StatusCode, resp.String())
	}

	if strings.TrimSpace(startResp.UserCode) == "" {
		startResp.UserCode = startResp.UserCodeAlt
	}
	if strings.TrimSpace(startResp.VerificationURL) == "" {
		startResp.VerificationURL = startResp.VerificationURI
	}
	if strings.TrimSpace(startResp.VerificationURL) == "" {
		startResp.VerificationURL = openai.DeviceVerificationURL
	}

	return &startResp, nil
}

func (s *openaiOAuthService) PollDeviceAuth(ctx context.Context, deviceAuthID, userCode, proxyURL string) (*openai.DeviceAuthTokenResponse, error) {
	client, err := createOpenAIReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_DEVICE_AUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	payload := map[string]string{
		"device_auth_id": deviceAuthID,
		"user_code":      userCode,
	}

	var tokenResp openai.DeviceAuthTokenResponse
	resp, err := client.R().
		SetContext(ctx).
		SetHeader("User-Agent", "codex-cli/0.91.0").
		SetHeader("Content-Type", "application/json").
		SetBody(payload).
		SetSuccessResult(&tokenResp).
		Post(s.deviceTokenURL)

	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_DEVICE_AUTH_REQUEST_FAILED", "request failed: %v", err)
	}
	if !resp.IsSuccessState() {
		return nil, openAIDeviceAuthPollError(resp.StatusCode, resp.String())
	}
	if strings.TrimSpace(tokenResp.Code) == "" {
		tokenResp.Code = tokenResp.AuthorizationCode
	}
	if strings.TrimSpace(tokenResp.CodeVerifier) == "" {
		tokenResp.CodeVerifier = tokenResp.CodeChallenge
	}
	if strings.TrimSpace(tokenResp.Code) == "" || strings.TrimSpace(tokenResp.CodeVerifier) == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_DEVICE_AUTH_INVALID_RESPONSE", "device auth response did not include authorization code")
	}

	return &tokenResp, nil
}

func (s *openaiOAuthService) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	client, err := createOpenAIReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	if redirectURI == "" {
		redirectURI = openai.DefaultRedirectURI
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = openai.ClientID
	}

	formData := url.Values{}
	formData.Set("grant_type", "authorization_code")
	formData.Set("client_id", clientID)
	formData.Set("code", code)
	formData.Set("redirect_uri", redirectURI)
	formData.Set("code_verifier", codeVerifier)

	var tokenResp openai.TokenResponse

	resp, err := client.R().
		SetContext(ctx).
		SetHeader("User-Agent", "codex-cli/0.91.0").
		SetFormDataFromValues(formData).
		SetSuccessResult(&tokenResp).
		Post(s.tokenURL)

	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_REQUEST_FAILED", "request failed: %v", err)
	}

	if !resp.IsSuccessState() {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_TOKEN_EXCHANGE_FAILED", "token exchange failed: status %d, body: %s", resp.StatusCode, resp.String())
	}

	return &tokenResp, nil
}

func (s *openaiOAuthService) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return s.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, "")
}

func (s *openaiOAuthService) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	// 调用方应始终传入正确的 client_id；为兼容旧数据，未指定时默认使用 OpenAI ClientID
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = openai.ClientID
	}
	return s.refreshTokenWithClientID(ctx, refreshToken, proxyURL, clientID)
}

func (s *openaiOAuthService) refreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL, clientID string) (*openai.TokenResponse, error) {
	client, err := createOpenAIReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	formData := url.Values{}
	formData.Set("grant_type", "refresh_token")
	formData.Set("refresh_token", refreshToken)
	formData.Set("client_id", clientID)
	formData.Set("scope", openai.RefreshScopes)

	var tokenResp openai.TokenResponse

	resp, err := client.R().
		SetContext(ctx).
		SetHeader("User-Agent", "codex-cli/0.91.0").
		SetFormDataFromValues(formData).
		SetSuccessResult(&tokenResp).
		Post(s.tokenURL)

	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_REQUEST_FAILED", "request failed: %v", err)
	}

	if !resp.IsSuccessState() {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_TOKEN_REFRESH_FAILED", "token refresh failed: status %d, body: %s", resp.StatusCode, resp.String())
	}

	return &tokenResp, nil
}

func createOpenAIReqClient(proxyURL string) (*req.Client, error) {
	return getSharedReqClient(reqClientOptions{
		ProxyURL: proxyURL,
		Timeout:  120 * time.Second,
	})
}

func shouldReturnOpenAINoProxyHint(ctx context.Context, proxyURL string, err error) bool {
	if strings.TrimSpace(proxyURL) != "" || err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, context.Canceled)
}

func newOpenAINoProxyHintError(cause error) error {
	return infraerrors.New(
		http.StatusBadGateway,
		"OPENAI_OAUTH_PROXY_REQUIRED",
		"OpenAI OAuth request failed: no proxy is configured and this server could not reach OpenAI directly. Select a proxy that can access OpenAI, then retry; if the authorization code has expired, regenerate the authorization URL.",
	).WithCause(cause)
}

func openAIDeviceAuthPollError(status int, body string) error {
	lowerBody := strings.ToLower(body)
	switch {
	case status == http.StatusUnauthorized && strings.Contains(lowerBody, "authorization_pending"):
		return infraerrors.New(http.StatusConflict, "OPENAI_DEVICE_AUTH_PENDING", "device authorization is still pending")
	case status == http.StatusTooManyRequests || strings.Contains(lowerBody, "slow_down"):
		return infraerrors.New(http.StatusTooManyRequests, "OPENAI_DEVICE_AUTH_SLOW_DOWN", "polling too quickly; wait before retrying")
	case status == http.StatusUnauthorized && strings.Contains(lowerBody, "expired"):
		return infraerrors.New(http.StatusBadRequest, "OPENAI_DEVICE_AUTH_EXPIRED", "device authorization code expired")
	case status == http.StatusUnauthorized && strings.Contains(lowerBody, "access_denied"):
		return infraerrors.New(http.StatusBadRequest, "OPENAI_DEVICE_AUTH_DENIED", "device authorization was denied")
	default:
		return infraerrors.Newf(http.StatusBadGateway, "OPENAI_DEVICE_AUTH_POLL_FAILED", "device auth poll failed: status %d, body: %s", status, body)
	}
}
