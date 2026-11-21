package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientRefreshStub struct {
	refreshCalls int32
}

type openAIRefreshProxyRepo struct {
	ProxyRepository
	proxy *Proxy
	err   error
}

func (r openAIRefreshProxyRepo) GetByID(context.Context, int64) (*Proxy, error) {
	return r.proxy, r.err
}

func TestOpenAIOAuthStoredAccountRequiresConfiguredProxy(t *testing.T) {
	proxyID := int64(11)
	account := &Account{
		ID: 77, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: &proxyID,
		Credentials: map[string]any{"access_token": "fixture-access", "refresh_token": "fixture-refresh"},
	}
	for _, test := range []struct {
		name string
		repo ProxyRepository
	}{
		{"repository unavailable", nil},
		{"proxy missing", openAIRefreshProxyRepo{}},
		{"read failure", openAIRefreshProxyRepo{err: errors.New("fixture unavailable")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &openaiOAuthClientRefreshStub{}
			svc := NewOpenAIOAuthService(test.repo, client)
			t.Cleanup(svc.Stop)
			_, err := svc.RefreshAccountToken(t.Context(), account)
			require.ErrorContains(t, err, "account proxy is unavailable")
			require.Zero(t, atomic.LoadInt32(&client.refreshCalls))
			privacyFactory := func(string) (*req.Client, error) {
				t.Fatal("privacy operation must wait for the configured proxy")
				return nil, nil
			}
			admin := &adminServiceImpl{proxyRepo: test.repo, privacyClientFactory: privacyFactory}
			require.Empty(t, admin.EnsureOpenAIPrivacy(t.Context(), account))
			require.Empty(t, admin.ForceOpenAIPrivacy(t.Context(), account))
			worker := &TokenRefreshService{proxyRepo: test.repo, privacyClientFactory: privacyFactory}
			worker.ensureOpenAIPrivacy(t.Context(), account)
		})
	}

	proxy := &Proxy{Host: "proxy.example", Port: 8080, Protocol: "http"}
	url, err := resolveOpenAIStoredAccountProxy(t.Context(), openAIRefreshProxyRepo{proxy: proxy}, &proxyID)
	require.NoError(t, err)
	require.Equal(t, proxy.URL(), url)
}

func (s *openaiOAuthClientRefreshStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientRefreshStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.refreshCalls, 1)
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientRefreshStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.refreshCalls, 1)
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientRefreshStub) StartDeviceAuth(ctx context.Context, proxyURL string) (*openai.DeviceAuthStartResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientRefreshStub) PollDeviceAuth(ctx context.Context, deviceAuthID, userCode, proxyURL string) (*openai.DeviceAuthTokenResponse, error) {
	return nil, errors.New("not implemented")
}

func TestOpenAIOAuthService_RefreshAccountToken_NoRefreshTokenUsesExistingAccessToken(t *testing.T) {
	client := &openaiOAuthClientRefreshStub{}
	svc := NewOpenAIOAuthService(nil, client)

	expiresAt := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	account := &Account{
		ID:       77,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "existing-access-token",
			"expires_at":   expiresAt,
			"client_id":    "client-id-1",
		},
	}

	info, err := svc.RefreshAccountToken(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "existing-access-token", info.AccessToken)
	require.Equal(t, "client-id-1", info.ClientID)
	require.Zero(t, atomic.LoadInt32(&client.refreshCalls), "existing access token should be reused without calling refresh")
}

func TestOpenAITokenRefresher_NeedsRefresh_SkipsAccountWithoutRefreshToken(t *testing.T) {
	refresher := NewOpenAITokenRefresher(nil, nil)
	expiresAt := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)

	withoutRT := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "access-token",
			"expires_at":   expiresAt,
		},
	}
	require.False(t, refresher.NeedsRefresh(withoutRT, 5*time.Minute))

	withRT := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"expires_at":    expiresAt,
		},
	}
	require.True(t, refresher.NeedsRefresh(withRT, 5*time.Minute))
}

func TestOpenAITokenProvider_NoRefreshTokenExpiredAccessTokenReturnsError(t *testing.T) {
	provider := NewOpenAITokenProvider(nil, nil, nil)
	expiresAt := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "expired-access-token",
			"expires_at":   expiresAt,
		},
	}

	token, err := provider.GetAccessToken(context.Background(), account)
	require.Error(t, err)
	require.Empty(t, token)
	require.Contains(t, err.Error(), "refresh_token is missing")
}
