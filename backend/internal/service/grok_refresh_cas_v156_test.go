//go:build unit

package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type grokRefreshCASRepoStub struct {
	mockAccountRepoForGemini
	account               *Account
	forceCASLoss          bool
	credentialCASCalls    int
	expectedCredentials   map[string]any
	expectedProxyID       *int64
	errorCASCalls         int
	errorCASWasApplied    bool
	tempUnschedCASCalls   int
	tempUnschedWasApplied bool
}

func cloneGrokRefreshTestAccount(account *Account) *Account {
	if account == nil {
		return nil
	}
	clone := *account
	clone.Credentials = cloneCredentials(account.Credentials)
	clone.ProxyID = cloneInt64Pointer(account.ProxyID)
	return &clone
}

func (r *grokRefreshCASRepoStub) GetByID(context.Context, int64) (*Account, error) {
	return cloneGrokRefreshTestAccount(r.account), nil
}

func (r *grokRefreshCASRepoStub) UpdateGrokOAuthCredentialsIfUnchanged(
	_ context.Context,
	_ int64,
	expectedCredentials map[string]any,
	expectedProxyID *int64,
	credentials map[string]any,
) (bool, error) {
	r.credentialCASCalls++
	r.expectedCredentials = cloneCredentials(expectedCredentials)
	r.expectedProxyID = cloneInt64Pointer(expectedProxyID)
	if r.forceCASLoss || r.account == nil ||
		!reflect.DeepEqual(r.account.Credentials, expectedCredentials) ||
		!sameInt64Pointer(r.account.ProxyID, expectedProxyID) {
		return false, nil
	}
	r.account.Credentials = cloneCredentials(credentials)
	return true, nil
}

func (r *grokRefreshCASRepoStub) SetGrokOAuthErrorIfCredentialsUnchanged(
	_ context.Context,
	_ int64,
	expectedCredentials map[string]any,
	expectedProxyID *int64,
	message string,
) (bool, error) {
	r.errorCASCalls++
	r.expectedCredentials = cloneCredentials(expectedCredentials)
	r.expectedProxyID = cloneInt64Pointer(expectedProxyID)
	applied := r.account != nil && r.account.Status == StatusActive && r.account.Schedulable &&
		reflect.DeepEqual(r.account.Credentials, expectedCredentials) &&
		sameInt64Pointer(r.account.ProxyID, expectedProxyID)
	r.errorCASWasApplied = applied
	if applied {
		r.account.Status = StatusError
		r.account.Schedulable = false
		r.account.ErrorMessage = message
	}
	return applied, nil
}

func (r *grokRefreshCASRepoStub) SetGrokOAuthTempUnschedulableIfCredentialsUnchanged(
	_ context.Context,
	_ int64,
	expectedCredentials map[string]any,
	expectedProxyID *int64,
	_ time.Time,
	_ string,
) (bool, error) {
	r.tempUnschedCASCalls++
	r.expectedCredentials = cloneCredentials(expectedCredentials)
	r.expectedProxyID = cloneInt64Pointer(expectedProxyID)
	applied := r.account != nil && reflect.DeepEqual(r.account.Credentials, expectedCredentials) &&
		sameInt64Pointer(r.account.ProxyID, expectedProxyID)
	r.tempUnschedWasApplied = applied
	return applied, nil
}

type grokRefreshCASExecutorStub struct {
	credentials map[string]any
	err         error
	onRefresh   func()
}

type grokRefreshTokenCacheStub struct {
	tokens      map[string]string
	deleteCalls int
}

func (c *grokRefreshTokenCacheStub) GetAccessToken(_ context.Context, key string) (string, error) {
	return c.tokens[key], nil
}

func (c *grokRefreshTokenCacheStub) SetAccessToken(_ context.Context, key, token string, _ time.Duration) error {
	if c.tokens == nil {
		c.tokens = make(map[string]string)
	}
	c.tokens[key] = token
	return nil
}

func (c *grokRefreshTokenCacheStub) DeleteAccessToken(_ context.Context, key string) error {
	c.deleteCalls++
	delete(c.tokens, key)
	return nil
}

func (c *grokRefreshTokenCacheStub) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

func (c *grokRefreshTokenCacheStub) ReleaseRefreshLock(context.Context, string) error { return nil }

func (e *grokRefreshCASExecutorStub) CanRefresh(account *Account) bool {
	return account != nil && account.IsGrokOAuth() && account.GetGrokRefreshToken() != ""
}

func (e *grokRefreshCASExecutorStub) NeedsRefresh(*Account, time.Duration) bool { return true }

func (e *grokRefreshCASExecutorStub) Refresh(context.Context, *Account) (map[string]any, error) {
	if e.onRefresh != nil {
		e.onRefresh()
	}
	return cloneCredentials(e.credentials), e.err
}

func (e *grokRefreshCASExecutorStub) CacheKey(account *Account) string {
	return GrokTokenCacheKey(account)
}

func newGrokRefreshCASTestAccount() *Account {
	proxyID := int64(7)
	return &Account{
		ID: 7001, Platform: PlatformGrok, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: &proxyID,
		Credentials: map[string]any{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"expires_at":    time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
		},
	}
}

func TestOAuthRefreshGrokCASSuccessReturnsDurableCredentials(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	repo := &grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}
	executor := &grokRefreshCASExecutorStub{credentials: map[string]any{
		"access_token":  "new-access",
		"refresh_token": "new-refresh",
		"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}

	result, err := NewOAuthRefreshAPI(repo, nil).RefreshIfNeeded(
		context.Background(), account, executor, grokTokenRefreshSkew,
	)

	require.NoError(t, err)
	require.True(t, result.Refreshed)
	require.Equal(t, 1, repo.credentialCASCalls)
	require.Equal(t, "old-refresh", repo.expectedCredentials["refresh_token"])
	require.Equal(t, "new-access", result.Account.GetGrokAccessToken())
	require.Equal(t, "new-refresh", result.Account.GetGrokRefreshToken())
}

func TestOAuthRefreshGrokCASLossKeepsConcurrentCredentials(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	repo := &grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}
	executor := &grokRefreshCASExecutorStub{
		credentials: map[string]any{"access_token": "provider-access", "refresh_token": "provider-refresh"},
		onRefresh: func() {
			repo.account.Credentials = map[string]any{
				"access_token":  "admin-access",
				"refresh_token": "admin-refresh",
				"expires_at":    time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			}
		},
	}

	result, err := NewOAuthRefreshAPI(repo, nil).RefreshIfNeeded(
		context.Background(), account, executor, grokTokenRefreshSkew,
	)

	require.NoError(t, err)
	require.False(t, result.Refreshed)
	require.Equal(t, "admin-access", result.Account.GetGrokAccessToken())
	require.Equal(t, "admin-refresh", repo.account.GetGrokRefreshToken())
	require.Equal(t, "old-refresh", repo.expectedCredentials["refresh_token"])
}

func TestGrokRefreshFailureDoesNotCooldownConcurrentCredentialRotation(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	repo := &grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}
	executor := &grokRefreshCASExecutorStub{
		err: errors.New("temporary refresh outage"),
		onRefresh: func() {
			// Simulate an admin rotation that wins while the provider call is in flight.
			repo.account.Credentials = map[string]any{
				"access_token":  "admin-access",
				"refresh_token": "admin-refresh",
			}
		},
	}
	provider := NewGrokTokenProvider(repo, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), executor)

	_, err := provider.GetAccessToken(context.Background(), account)

	require.Error(t, err)
	require.Equal(t, 1, repo.tempUnschedCASCalls)
	require.Equal(t, "old-refresh", repo.expectedCredentials["refresh_token"])
	require.False(t, repo.tempUnschedWasApplied)
	require.Equal(t, "admin-refresh", repo.account.GetGrokRefreshToken())
}

func TestGrokBackgroundRefreshFailureDoesNotCooldownConcurrentCredentialRotation(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	repo := &grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}
	executor := &grokRefreshCASExecutorStub{
		err: errors.New("temporary refresh outage"),
		onRefresh: func() {
			repo.account.Credentials = map[string]any{
				"access_token":  "admin-access",
				"refresh_token": "admin-refresh",
				"expires_at":    time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			}
		},
	}
	cfg := &config.TokenRefreshConfig{MaxRetries: 1}
	service := &TokenRefreshService{
		accountRepo: repo,
		refreshAPI:  NewOAuthRefreshAPI(repo, nil),
		cfg:         cfg,
	}

	err := service.refreshWithRetry(context.Background(), account, executor, executor, grokTokenRefreshSkew)

	require.Error(t, err)
	require.Equal(t, 1, repo.tempUnschedCASCalls)
	require.False(t, repo.tempUnschedWasApplied)
	require.Equal(t, "admin-refresh", repo.account.GetGrokRefreshToken())
}

func TestGrokBackgroundNonRetryableRefreshDoesNotBlockConcurrentProxyChange(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	repo := &grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}
	executor := &grokRefreshCASExecutorStub{
		err: errors.New("invalid_grant"),
		onRefresh: func() {
			proxyID := int64(99)
			repo.account.ProxyID = &proxyID
		},
	}
	cfg := &config.TokenRefreshConfig{MaxRetries: 1}
	service := &TokenRefreshService{
		accountRepo: repo,
		refreshAPI:  NewOAuthRefreshAPI(repo, nil),
		cfg:         cfg,
	}

	err := service.refreshWithRetry(context.Background(), account, executor, executor, grokTokenRefreshSkew)

	require.Error(t, err)
	require.Equal(t, 1, repo.errorCASCalls)
	require.False(t, repo.errorCASWasApplied)
	require.Equal(t, int64(99), *repo.account.ProxyID)
	require.Equal(t, StatusActive, repo.account.Status)
}

func TestGrokTokenProviderClientCancellationDoesNotCooldownAccount(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	repo := &grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}
	ctx, cancel := context.WithCancel(context.Background())
	executor := &grokRefreshCASExecutorStub{
		err: errors.New("refresh interrupted"),
		onRefresh: func() {
			cancel()
		},
	}
	provider := NewGrokTokenProvider(repo, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), executor)

	token, err := provider.GetAccessToken(ctx, account)

	require.Empty(t, token)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, repo.tempUnschedCASCalls)
	require.Zero(t, repo.errorCASCalls)
}

func TestGrokWaitForRefreshedTokenRejectsProxyChange(t *testing.T) {
	selected := newGrokRefreshCASTestAccount()
	latest := cloneGrokRefreshTestAccount(selected)
	changedProxyID := int64(8)
	latest.ProxyID = &changedProxyID
	latest.Credentials = map[string]any{
		"access_token":   "new-access",
		"refresh_token":  "new-refresh",
		"expires_at":     time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"_token_version": time.Now().UnixMilli(),
	}
	provider := NewGrokTokenProvider(&grokRefreshCASRepoStub{account: latest}, nil)

	token, err := provider.waitForRefreshedToken(
		context.Background(), selected, cloneInt64Pointer(selected.ProxyID), GrokTokenCacheKey(selected),
	)

	require.Empty(t, token)
	require.ErrorIs(t, err, errOAuthRefreshAccountStateChanged)
}

func TestGrokWaitForRefreshedTokenRejectsDisabledAccount(t *testing.T) {
	selected := newGrokRefreshCASTestAccount()
	latest := cloneGrokRefreshTestAccount(selected)
	latest.Status = StatusError
	latest.Schedulable = false
	latest.Credentials = map[string]any{
		"access_token":   "new-access",
		"refresh_token":  "new-refresh",
		"expires_at":     time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"_token_version": time.Now().UnixMilli(),
	}
	provider := NewGrokTokenProvider(&grokRefreshCASRepoStub{account: latest}, nil)

	token, err := provider.waitForRefreshedToken(
		context.Background(), selected, cloneInt64Pointer(selected.ProxyID), GrokTokenCacheKey(selected),
	)

	require.Empty(t, token)
	require.ErrorIs(t, err, errOAuthRefreshAccountStateChanged)
}

func TestGrokTokenProviderRefreshesMissingAccessToken(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	delete(account.Credentials, "access_token")
	delete(account.Credentials, "expires_at")
	repo := &grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}
	executor := &grokRefreshCASExecutorStub{credentials: map[string]any{
		"access_token":  "new-access",
		"refresh_token": "new-refresh",
		"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}
	provider := NewGrokTokenProvider(repo, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), executor)

	token, err := provider.GetAccessToken(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "new-access", token)
	require.Equal(t, 1, repo.credentialCASCalls)
}

func TestGrokTokenProviderUsesUnexpiredNonRenewableSSOToken(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	delete(account.Credentials, "refresh_token")
	account.Credentials["expires_at"] = time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)
	repo := &grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}
	provider := NewGrokTokenProvider(repo, nil)

	token, err := provider.GetAccessToken(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "old-access", token)
}

func TestGrokTokenProviderProbeCanInspectDynamicallyRateLimitedAccount(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	account.Credentials["expires_at"] = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	resetAt := time.Now().Add(30 * time.Minute)
	account.RateLimitResetAt = &resetAt
	provider := NewGrokTokenProvider(&grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}, nil)

	normalToken, normalErr := provider.GetAccessToken(context.Background(), account)
	probeToken, probeErr := provider.GetAccessTokenForProbe(context.Background(), account)

	require.Empty(t, normalToken)
	require.ErrorIs(t, normalErr, errOAuthRefreshAccountStateChanged)
	require.NoError(t, probeErr)
	require.Equal(t, "old-access", probeToken)
}

func TestGrokTokenProviderProbeStillRejectsExpiredAutoPausedAccount(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	expiredAt := time.Now().Add(-time.Minute)
	account.AutoPauseOnExpired = true
	account.ExpiresAt = &expiredAt
	provider := NewGrokTokenProvider(&grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}, nil)

	token, err := provider.GetAccessTokenForProbe(context.Background(), account)

	require.Empty(t, token)
	require.ErrorIs(t, err, errOAuthRefreshAccountStateChanged)
}

func TestGrokTokenProviderManualTestBypassesSchedulingState(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	account.ProxyID = nil
	account.Schedulable = false
	resetAt := time.Now().Add(time.Hour)
	account.RateLimitResetAt = &resetAt
	account.Credentials["expires_at"] = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	provider := NewGrokTokenProvider(nil, nil)

	token, err := provider.GetAccessTokenForManualTest(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "old-access", token)
}

func TestGrokTokenProviderManualTestRejectsMissingConfiguredProxy(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	account.Credentials["expires_at"] = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	provider := NewGrokTokenProvider(nil, nil)

	_, err := provider.GetAccessTokenForManualTest(context.Background(), account)

	require.ErrorIs(t, err, errGrokOAuthConfiguredProxyMiss)
}

func TestGrokRefreshErrorRecoveryRejectsProxyChange(t *testing.T) {
	used := newGrokRefreshCASTestAccount()
	latest := cloneGrokRefreshTestAccount(used)
	changedProxyID := int64(8)
	latest.ProxyID = &changedProxyID
	latest.Credentials = map[string]any{
		"access_token":   "admin-access",
		"refresh_token":  "admin-refresh",
		"expires_at":     time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"_token_version": time.Now().UnixMilli(),
	}
	provider := NewGrokTokenProvider(&grokRefreshCASRepoStub{account: latest}, nil)

	token, recovered, err := provider.recoverAfterRefreshError(
		context.Background(), used, cloneInt64Pointer(used.ProxyID),
	)

	require.Empty(t, token)
	require.NotNil(t, recovered)
	require.Equal(t, "admin-access", recovered.GetGrokAccessToken())
	require.ErrorIs(t, err, errOAuthRefreshAccountStateChanged)
}

func TestGrokRefreshCASLossRejectsChangedProxyResult(t *testing.T) {
	selected := newGrokRefreshCASTestAccount()
	repo := &grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(selected)}
	executor := &grokRefreshCASExecutorStub{
		credentials: map[string]any{
			"access_token":  "provider-access",
			"refresh_token": "provider-refresh",
			"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
		onRefresh: func() {
			changedProxyID := int64(8)
			repo.account.ProxyID = &changedProxyID
			repo.account.Credentials = map[string]any{
				"access_token":  "admin-access",
				"refresh_token": "admin-refresh",
				"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			}
		},
	}
	provider := NewGrokTokenProvider(repo, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), executor)

	token, err := provider.GetAccessToken(context.Background(), selected)

	require.Empty(t, token)
	require.ErrorIs(t, err, errOAuthRefreshAccountStateChanged)
}

func TestGrokPermanentRefreshFailureInvalidatesTokenCache(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	repo := &grokRefreshCASRepoStub{account: cloneGrokRefreshTestAccount(account)}
	cacheKey := GrokTokenCacheKey(account)
	cache := &grokRefreshTokenCacheStub{tokens: map[string]string{cacheKey: "stale-access"}}
	provider := NewGrokTokenProvider(repo, cache)

	provider.markTempUnschedulable(account, errors.New("invalid_grant"))

	require.Equal(t, 1, repo.errorCASCalls)
	require.True(t, repo.errorCASWasApplied)
	require.Equal(t, StatusError, repo.account.Status)
	require.Equal(t, 1, cache.deleteCalls)
	require.NotContains(t, cache.tokens, cacheKey)
}
