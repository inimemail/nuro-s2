package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type forceRefreshCacheStub struct {
	tokens     map[string]string
	lockResult bool
}

func (s *forceRefreshCacheStub) GetAccessToken(_ context.Context, cacheKey string) (string, error) {
	return s.tokens[cacheKey], nil
}

func (s *forceRefreshCacheStub) SetAccessToken(_ context.Context, cacheKey string, token string, _ time.Duration) error {
	if s.tokens == nil {
		s.tokens = map[string]string{}
	}
	s.tokens[cacheKey] = token
	return nil
}

func (s *forceRefreshCacheStub) DeleteAccessToken(_ context.Context, cacheKey string) error {
	delete(s.tokens, cacheKey)
	return nil
}

func (s *forceRefreshCacheStub) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return s.lockResult, nil
}

func (s *forceRefreshCacheStub) ReleaseRefreshLock(context.Context, string) error {
	return nil
}

type forceRefreshExecutorStub struct {
	refreshCalls int
}

func (s *forceRefreshExecutorStub) CanRefresh(*Account) bool {
	return true
}

func (s *forceRefreshExecutorStub) NeedsRefresh(*Account, time.Duration) bool {
	return true
}

func (s *forceRefreshExecutorStub) Refresh(context.Context, *Account) (map[string]any, error) {
	s.refreshCalls++
	return map[string]any{"access_token": "new-token"}, nil
}

func (s *forceRefreshExecutorStub) CacheKey(account *Account) string {
	return OpenAITokenCacheKey(account)
}

func TestOpenAITokenProvider_ForceRefreshLockHeldDoesNotReturnOldToken(t *testing.T) {
	account := &Account{
		ID:       9101,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "refresh-token",
		},
	}
	cacheKey := OpenAITokenCacheKey(account)
	cache := &forceRefreshCacheStub{
		tokens: map[string]string{
			cacheKey: "old-token",
		},
		lockResult: false,
	}
	executor := &forceRefreshExecutorStub{}
	provider := NewOpenAITokenProvider(nil, cache, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(nil, cache), executor)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	refreshedAccount, token, err := provider.ForceRefresh(ctx, account)

	require.Error(t, err)
	require.Nil(t, refreshedAccount)
	require.Empty(t, token)
	require.NotContains(t, strings.ToLower(err.Error()), "old-token")
	require.Equal(t, 0, executor.refreshCalls)
}
