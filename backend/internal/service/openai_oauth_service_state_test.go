package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientStateStub struct {
	exchangeCalled  int32
	lastClientID    string
	lastRedirectURI string
	exchangeResp    *openai.TokenResponse
	pollDeviceAuth  func(ctx context.Context, deviceAuthID, userCode, proxyURL string) (*openai.DeviceAuthTokenResponse, error)
}

func (s *openaiOAuthClientStateStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.exchangeCalled, 1)
	s.lastClientID = clientID
	s.lastRedirectURI = redirectURI
	if s.exchangeResp != nil {
		return s.exchangeResp, nil
	}
	return &openai.TokenResponse{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresIn:    3600,
	}, nil
}

func (s *openaiOAuthClientStateStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientStateStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	return s.RefreshToken(ctx, refreshToken, proxyURL)
}

func (s *openaiOAuthClientStateStub) StartDeviceAuth(ctx context.Context, proxyURL string) (*openai.DeviceAuthStartResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientStateStub) PollDeviceAuth(ctx context.Context, deviceAuthID, userCode, proxyURL string) (*openai.DeviceAuthTokenResponse, error) {
	if s.pollDeviceAuth != nil {
		return s.pollDeviceAuth(ctx, deviceAuthID, userCode, proxyURL)
	}
	return nil, errors.New("not implemented")
}

func TestOpenAIOAuthService_ExchangeCode_StateRequired(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()

	svc.sessionStore.Set("sid", &openai.OAuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	})

	_, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "oauth state is required")
	require.Equal(t, int32(0), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthService_ExchangeCode_StateMismatch(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()

	svc.sessionStore.Set("sid", &openai.OAuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	})

	_, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
		State:     "wrong-state",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid oauth state")
	require.Equal(t, int32(0), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthService_ExchangeCode_StateMatch(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()

	svc.sessionStore.Set("sid", &openai.OAuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	})

	info, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
		State:     "expected-state",
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "at", info.AccessToken)
	require.Equal(t, openai.ClientID, info.ClientID)
	require.Equal(t, openai.ClientID, client.lastClientID)
	require.Equal(t, int32(1), atomic.LoadInt32(&client.exchangeCalled))

	_, ok := svc.sessionStore.Get("sid")
	require.False(t, ok)
}

func TestOpenAIOAuthService_ExchangeCode_NoRefreshTokenAllowedAsAccessTokenOnly(t *testing.T) {
	client := &openaiOAuthClientStateStub{
		exchangeResp: &openai.TokenResponse{
			AccessToken: "at",
			ExpiresIn:   3600,
		},
	}
	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()

	svc.sessionStore.Set("sid", &openai.OAuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	})

	info, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
		State:     "expected-state",
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "at", info.AccessToken)
	require.Empty(t, info.RefreshToken)
	require.Equal(t, int32(1), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthService_DeviceAuth_ExchangesWithDeviceRedirect(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()

	svc.sessionStore.Set("device-sid", &openai.OAuthSession{
		ClientID:       openai.ClientID,
		RedirectURI:    openai.DeviceRedirectURI,
		DeviceAuthID:   "device-auth-id",
		DeviceUserCode: "ABCD-EFGH",
		CreatedAt:      time.Now(),
	})

	client.pollDeviceAuth = func(ctx context.Context, deviceAuthID, userCode, proxyURL string) (*openai.DeviceAuthTokenResponse, error) {
		return &openai.DeviceAuthTokenResponse{
			Code:         "device-auth-code",
			CodeVerifier: "device-verifier",
		}, nil
	}

	info, err := svc.ExchangeDeviceAuth(context.Background(), "device-sid", nil)
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "at", info.AccessToken)
	require.Equal(t, openai.DeviceRedirectURI, client.lastRedirectURI)
}
