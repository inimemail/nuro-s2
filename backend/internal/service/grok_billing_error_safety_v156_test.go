//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type grokBillingErrorRepo struct {
	mockAccountRepoForGemini
	account *Account
}

func (r *grokBillingErrorRepo) GetByID(context.Context, int64) (*Account, error) {
	return cloneGrokRefreshTestAccount(r.account), nil
}

type grokBillingErrorUpstream struct{}

func (grokBillingErrorUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusBadGateway,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Location":     []string{"https://private-provider.example/error"},
			"Server":       []string{"private-provider-edge"},
		},
		Body: io.NopCloser(strings.NewReader(
			`<!DOCTYPE html><title>private-provider.example | 502</title><a href="https://www.cloudflare.com/error">error</a>`,
		)),
	}, nil
}

func (u grokBillingErrorUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestGrokBillingProbeErrorDoesNotExposeResponseBody(t *testing.T) {
	account := &Account{
		ID: 8001, Platform: PlatformGrok, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"expires_at":    time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
	repo := &grokBillingErrorRepo{account: account}
	svc := NewGrokQuotaService(repo, nil, NewGrokTokenProvider(repo, nil), grokBillingErrorUpstream{})

	result, err := svc.ProbeBilling(context.Background(), account.ID)

	require.Error(t, err)
	require.Nil(t, result)
	for _, secret := range []string{"private-provider", "cloudflare", "DOCTYPE", "Location", "Server"} {
		require.NotContains(t, strings.ToLower(err.Error()), strings.ToLower(secret))
	}
}

type grokQuotaRecoveryRepo struct {
	mockAccountRepoForGemini
	account    *Account
	clearCalls int
}

func (r *grokQuotaRecoveryRepo) GetByID(context.Context, int64) (*Account, error) {
	return cloneGrokRefreshTestAccount(r.account), nil
}

func (r *grokQuotaRecoveryRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	return nil
}

func (r *grokQuotaRecoveryRepo) ClearRateLimitIfObserved(context.Context, int64, time.Time, time.Time) (bool, error) {
	r.clearCalls++
	return true, nil
}

type grokQuotaRecoveryUpstream struct{}

func (grokQuotaRecoveryUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}, nil
}

func (u grokQuotaRecoveryUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestGrokActiveProbeRecoveryClearsRuntimeRateLimit(t *testing.T) {
	limitedAt := time.Now().Add(-time.Minute)
	resetAt := time.Now().Add(10 * time.Minute)
	account := &Account{
		ID: 8002, Platform: PlatformGrok, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		RateLimitedAt: &limitedAt, RateLimitResetAt: &resetAt,
		Credentials: map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"expires_at":    time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
	repo := &grokQuotaRecoveryRepo{account: account}
	runtimeState := &OpenAIGatewayService{}
	runtimeState.BlockAccountScheduling(account, resetAt, "429")
	require.True(t, runtimeState.isOpenAIAccountRuntimeBlocked(account))
	quota := NewGrokQuotaService(repo, nil, NewGrokTokenProvider(repo, nil), grokQuotaRecoveryUpstream{})
	quota.SetRuntimeRecovery(runtimeState)

	result, err := quota.ProbeUsage(context.Background(), account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, repo.clearCalls)
	require.False(t, runtimeState.isOpenAIAccountRuntimeBlocked(account))
}
