package repository

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	sharedhttp "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/imroc/req/v3"
)

type grokOAuthClient struct {
	tokenURL string
}

func NewGrokOAuthClient() service.GrokOAuthClient {
	return &grokOAuthClient{tokenURL: xai.EffectiveTokenURL()}
}

func (c *grokOAuthClient) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*xai.TokenResponse, error) {
	client, err := createGrokReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_OAUTH_CLIENT_INIT_FAILED", "failed to initialize OAuth client").WithCause(err)
	}

	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = xai.EffectiveClientID()
	}

	formData := url.Values{}
	formData.Set("grant_type", "authorization_code")
	formData.Set("client_id", clientID)
	formData.Set("code", code)
	formData.Set("redirect_uri", xai.EffectiveRedirectURI(redirectURI))
	formData.Set("code_verifier", codeVerifier)

	var tokenResp xai.TokenResponse
	resp, err := client.R().
		SetContext(ctx).
		SetHeader("User-Agent", "sub2api-grok-oauth/1.0").
		SetFormDataFromValues(formData).
		SetSuccessResult(&tokenResp).
		Post(c.tokenURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_OAUTH_REQUEST_FAILED", "OAuth request failed").WithCause(err)
	}
	if !resp.IsSuccessState() {
		return nil, grokOAuthStatusError("GROK_OAUTH_TOKEN_EXCHANGE_FAILED", "token exchange failed", resp)
	}
	return &tokenResp, nil
}

func (c *grokOAuthClient) RefreshToken(ctx context.Context, refreshToken, proxyURL, clientID string) (*xai.TokenResponse, error) {
	client, err := createGrokReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_OAUTH_CLIENT_INIT_FAILED", "failed to initialize OAuth client").WithCause(err)
	}

	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = xai.EffectiveClientID()
	}

	formData := url.Values{}
	formData.Set("grant_type", "refresh_token")
	formData.Set("client_id", clientID)
	formData.Set("refresh_token", refreshToken)

	var tokenResp xai.TokenResponse
	resp, err := client.R().
		SetContext(ctx).
		SetHeader("User-Agent", "sub2api-grok-oauth/1.0").
		SetFormDataFromValues(formData).
		SetSuccessResult(&tokenResp).
		Post(c.tokenURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_OAUTH_REQUEST_FAILED", "OAuth request failed").WithCause(err)
	}
	if !resp.IsSuccessState() {
		return nil, grokOAuthStatusError("GROK_OAUTH_TOKEN_REFRESH_FAILED", "token refresh failed", resp)
	}
	return &tokenResp, nil
}

func (c *grokOAuthClient) ConvertSSOToBuild(ctx context.Context, ssoToken, proxyURL string) (*xai.TokenResponse, error) {
	client, err := createGrokSSOHTTPClient(proxyURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_SSO_CLIENT_INIT_FAILED", "failed to create HTTP client")
	}
	requestCtx, cancel := context.WithTimeout(ctx, xai.SSOConversionTimeout)
	defer cancel()
	tokenResp, err := xai.ConvertSSOToBuild(requestCtx, ssoToken, &xai.SSODeviceOptions{HTTPClient: client})
	if err != nil {
		return nil, grokSSOConversionError(err)
	}
	return tokenResp, nil
}

func createGrokReqClient(proxyURL string) (*req.Client, error) {
	return getSharedReqClient(reqClientOptions{
		ProxyURL: proxyURL,
		Timeout:  60 * time.Second,
	})
}

func createGrokSSOHTTPClient(proxyURL string) (*http.Client, error) {
	client, err := sharedhttp.GetClient(sharedhttp.Options{
		ProxyURL:              proxyURL,
		Timeout:               xai.SSOConversionTimeout,
		ResponseHeaderTimeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone, nil
}

func grokSSOConversionError(err error) error {
	switch {
	case errors.Is(err, xai.ErrSSOUnauthorized):
		return infraerrors.New(http.StatusUnauthorized, "GROK_SSO_UNAUTHORIZED", "SSO credential is invalid or expired")
	case errors.Is(err, xai.ErrSSOAuthorizationDenied):
		return infraerrors.New(http.StatusForbidden, "GROK_SSO_AUTHORIZATION_DENIED", "device authorization was denied or expired")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return infraerrors.New(http.StatusGatewayTimeout, "GROK_SSO_TIMEOUT", "SSO conversion timed out")
	}
	var statusErr xai.SSOHTTPError
	if errors.As(err, &statusErr) {
		statusCode := http.StatusBadGateway
		if statusErr.Status == http.StatusForbidden {
			statusCode = http.StatusForbidden
		}
		return infraerrors.New(statusCode, "GROK_SSO_UPSTREAM_FAILED", "SSO conversion failed")
	}
	return infraerrors.New(http.StatusBadGateway, "GROK_SSO_CONVERSION_FAILED", "SSO conversion failed")
}

func grokOAuthStatusError(code, message string, resp *req.Response) error {
	upstreamStatus := 0
	body := ""
	if resp != nil {
		upstreamStatus = resp.StatusCode
		body = resp.String()
	}
	return newGrokOAuthStatusError(code, message, upstreamStatus, body)
}

func newGrokOAuthStatusError(code, message string, upstreamStatus int, body string) error {
	statusCode := http.StatusBadGateway
	errorCode := code
	if upstreamStatus == http.StatusForbidden && grokOAuthHasExplicitEntitlementDenial(body) {
		statusCode = http.StatusForbidden
		errorCode = "GROK_OAUTH_ENTITLEMENT_DENIED"
	}
	if identifier := grokOAuthSafeErrorIdentifier(body); identifier != "" {
		return infraerrors.Newf(statusCode, errorCode, "%s: status %d (%s)", message, upstreamStatus, identifier)
	}
	return infraerrors.Newf(statusCode, errorCode, "%s: status %d", message, upstreamStatus)
}

func grokOAuthSafeErrorIdentifier(body string) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &payload) != nil {
		return ""
	}
	for _, field := range []string{"error", "code", "reason"} {
		raw, ok := payload[field]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			continue
		}
		value = strings.ToLower(strings.TrimSpace(value))
		if _, ok := grokOAuthSafeErrorIdentifiers[value]; ok {
			return value
		}
	}
	return ""
}

var grokOAuthSafeErrorIdentifiers = map[string]struct{}{
	"access_denied":             {},
	"entitlement_denied":        {},
	"invalid_client":            {},
	"invalid_grant":             {},
	"invalid_request":           {},
	"invalid_scope":             {},
	"no_active_subscription":    {},
	"refresh_token_expired":     {},
	"refresh_token_invalidated": {},
	"refresh_token_reused":      {},
	"subscription_required":     {},
	"unauthorized_client":       {},
	"unsupported_grant_type":    {},
}

func grokOAuthHasExplicitEntitlementDenial(body string) bool {
	lower := strings.ToLower(body)
	compact := strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "").Replace(lower)
	for _, field := range []string{"error", "code", "reason"} {
		for _, value := range []string{"access_denied", "entitlement_denied", "subscription_required", "no_active_subscription"} {
			if strings.Contains(compact, `"`+field+`":"`+value+`"`) {
				return true
			}
		}
	}
	return strings.Contains(lower, "entitlement denied") ||
		strings.Contains(lower, "subscription required") ||
		strings.Contains(lower, "no active grok subscription")
}
