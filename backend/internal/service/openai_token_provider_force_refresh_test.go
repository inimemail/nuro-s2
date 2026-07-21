package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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
	refreshCalls  int
	lastAccountID int64
	refreshErr    error
}

func (s *forceRefreshExecutorStub) CanRefresh(*Account) bool {
	return true
}

func (s *forceRefreshExecutorStub) NeedsRefresh(*Account, time.Duration) bool {
	return true
}

func (s *forceRefreshExecutorStub) Refresh(_ context.Context, account *Account) (map[string]any, error) {
	s.refreshCalls++
	if account != nil {
		s.lastAccountID = account.ID
	}
	if s.refreshErr != nil {
		return nil, s.refreshErr
	}
	return map[string]any{
		"access_token":  "new-token",
		"refresh_token": "new-refresh-token",
	}, nil
}

func (s *forceRefreshExecutorStub) CacheKey(account *Account) string {
	return OpenAITokenCacheKey(account)
}

type forceRefreshAccountRepoStub struct {
	AccountRepository
	accounts        map[int64]*Account
	setErrorCalls   int
	setErrorID      int64
	setErrorMessage string
}

func (s *forceRefreshAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if s.accounts == nil {
		return nil, errors.New("account not found")
	}
	account := s.accounts[id]
	if account == nil {
		return nil, errors.New("account not found")
	}
	return account, nil
}

func (s *forceRefreshAccountRepoStub) Update(_ context.Context, account *Account) error {
	if s.accounts == nil {
		s.accounts = map[int64]*Account{}
	}
	s.accounts[account.ID] = account
	return nil
}

func (s *forceRefreshAccountRepoStub) SetError(_ context.Context, id int64, errorMsg string) error {
	s.setErrorCalls++
	s.setErrorID = id
	s.setErrorMessage = errorMsg
	return nil
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

func TestOpenAITokenProvider_ForceRefreshShadowUsesParentCredentials(t *testing.T) {
	parentID := int64(9201)
	parent := &Account{
		ID:       parentID,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "old-parent-token",
			"refresh_token": "old-parent-refresh",
		},
	}
	shadow := &Account{
		ID:              9202,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5": "gpt-5-thinking"},
		},
	}
	repo := &forceRefreshAccountRepoStub{accounts: map[int64]*Account{
		parent.ID: parent,
		shadow.ID: shadow,
	}}
	cache := &forceRefreshCacheStub{tokens: map[string]string{
		OpenAITokenCacheKey(parent): "old-parent-token",
		OpenAITokenCacheKey(shadow): "shadow-cache-should-not-be-used",
	}, lockResult: true}
	executor := &forceRefreshExecutorStub{}
	provider := NewOpenAITokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, cache), executor)

	refreshedAccount, token, err := provider.ForceRefresh(context.Background(), shadow)

	require.NoError(t, err)
	require.Equal(t, "new-token", token)
	require.NotNil(t, refreshedAccount)
	require.Equal(t, parent.ID, refreshedAccount.ID)
	require.Equal(t, 1, executor.refreshCalls)
	require.Equal(t, parent.ID, executor.lastAccountID)
	require.Equal(t, "new-token", parent.GetOpenAIAccessToken())
	require.Equal(t, "new-refresh-token", parent.GetOpenAIRefreshToken())
	require.Empty(t, shadow.GetOpenAIAccessToken())
	require.Equal(t, "new-token", cache.tokens[OpenAITokenCacheKey(parent)])
	require.Equal(t, "shadow-cache-should-not-be-used", cache.tokens[OpenAITokenCacheKey(shadow)])
}

func TestOpenAIGatewayService_TryRecoverOpenAIOAuth401KeepsShadowForRetry(t *testing.T) {
	setGinTestMode()
	parentID := int64(9301)
	parent := &Account{
		ID:       parentID,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "old-parent-token",
			"refresh_token": "old-parent-refresh",
		},
	}
	shadow := &Account{
		ID:              9302,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
	}
	repo := &forceRefreshAccountRepoStub{accounts: map[int64]*Account{
		parent.ID: parent,
		shadow.ID: shadow,
	}}
	cache := &forceRefreshCacheStub{tokens: map[string]string{}, lockResult: true}
	executor := &forceRefreshExecutorStub{}
	provider := NewOpenAITokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, cache), executor)
	svc := &OpenAIGatewayService{openAITokenProvider: provider}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	retryAccount, token, ok := svc.tryRecoverOpenAIOAuth401(
		context.Background(),
		c,
		shadow,
		http.StatusUnauthorized,
		[]byte(`{"error":{"code":"expired_token"}}`),
	)

	require.True(t, ok)
	require.Equal(t, "new-token", token)
	require.NotNil(t, retryAccount)
	require.Equal(t, shadow.ID, retryAccount.ID)
	require.Equal(t, parent.ID, executor.lastAccountID)
	require.Equal(t, "new-token", parent.GetOpenAIAccessToken())
}

func TestOpenAIGatewayService_TryRecoverOpenAIOAuth401MarksParentOnShadowRefreshFailure(t *testing.T) {
	setGinTestMode()
	parentID := int64(9401)
	parent := &Account{
		ID:       parentID,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "old-parent-token",
			"refresh_token": "old-parent-refresh",
		},
	}
	shadow := &Account{
		ID:              9402,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
	}
	repo := &forceRefreshAccountRepoStub{accounts: map[int64]*Account{
		parent.ID: parent,
		shadow.ID: shadow,
	}}
	cache := &forceRefreshCacheStub{tokens: map[string]string{}, lockResult: true}
	executor := &forceRefreshExecutorStub{refreshErr: errors.New("invalid_grant")}
	provider := NewOpenAITokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, cache), executor)
	svc := &OpenAIGatewayService{
		accountRepo:         repo,
		openAITokenProvider: provider,
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	retryAccount, token, ok := svc.tryRecoverOpenAIOAuth401(
		context.Background(),
		c,
		shadow,
		http.StatusUnauthorized,
		[]byte(`{"error":{"code":"expired_token"}}`),
	)

	require.False(t, ok)
	require.Nil(t, retryAccount)
	require.Empty(t, token)
	require.Equal(t, 1, executor.refreshCalls)
	require.Equal(t, parent.ID, executor.lastAccountID)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Equal(t, parent.ID, repo.setErrorID)
	require.Contains(t, repo.setErrorMessage, "reauthorization required")
}
