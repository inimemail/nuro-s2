package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OAuthRefreshExecutor 各平台实现的 OAuth 刷新执行器
// TokenRefresher 接口的超集：增加了 CacheKey 方法用于分布式锁
type OAuthRefreshExecutor interface {
	TokenRefresher

	// CacheKey 返回用于分布式锁的缓存键（与 TokenProvider 使用的一致）
	CacheKey(account *Account) string
}

type GrokOAuthRefreshSuccessRepository interface {
	UpdateGrokOAuthCredentialsIfUnchanged(
		ctx context.Context,
		id int64,
		expectedCredentials map[string]any,
		expectedProxyID *int64,
		credentials map[string]any,
	) (bool, error)
}

type OpenAIOAuthRefreshSuccessRepository interface {
	UpdateOpenAIOAuthCredentialsIfUnchanged(ctx context.Context, id int64, expectedCredentials map[string]any, expectedProxyID *int64, credentials map[string]any) (bool, error)
}

const (
	defaultRefreshLockTTL            = 60 * time.Second
	defaultRefreshLockReleaseTimeout = 2 * time.Second
	defaultRefreshPersistenceTimeout = 5 * time.Second
)

var (
	errOpenAIRotationPersistence       = errors.New("OpenAI OAuth rotation persistence failed")
	errOAuthRefreshAccountRereadFailed = errors.New("oauth refresh account reread failed")
	errOAuthRefreshAccountStateChanged = errors.New("oauth refresh account state changed")
)

type contextMutex struct {
	token chan struct{}
}

func newContextMutex() *contextMutex {
	return &contextMutex{token: make(chan struct{}, 1)}
}

func (m *contextMutex) Lock(ctx context.Context) error {
	select {
	case m.token <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *contextMutex) Unlock() {
	<-m.token
}

// OAuthRefreshResult 统一刷新结果
type OAuthRefreshResult struct {
	Refreshed      bool           // 实际执行了刷新
	NewCredentials map[string]any // 刷新后的 credentials（nil 表示未刷新）
	Account        *Account       // 从 DB 重新读取的最新 account
	LockHeld       bool           // 锁被其他 worker 持有（未执行刷新）
}

func snapshotOAuthRefreshAccount(account *Account) *Account {
	if account == nil {
		return nil
	}
	snapshot := *account
	snapshot.Credentials = cloneCredentials(account.Credentials)
	snapshot.ProxyID = cloneInt64Pointer(account.ProxyID)
	return &snapshot
}

// OAuthRefreshAPI 统一的 OAuth Token 刷新入口
// 封装分布式锁、进程内互斥锁、DB 重读、已刷新检查、竞争恢复等通用逻辑
type OAuthRefreshAPI struct {
	accountRepo AccountRepository
	tokenCache  GeminiTokenCache // 可选，nil = 无分布式锁
	lockTTL     time.Duration
	localLocks  sync.Map // key: cacheKey string -> value: *contextMutex
}

// NewOAuthRefreshAPI 创建统一刷新 API
// 可选传入 lockTTL 覆盖默认的 60s 分布式锁 TTL
func NewOAuthRefreshAPI(accountRepo AccountRepository, tokenCache GeminiTokenCache, lockTTL ...time.Duration) *OAuthRefreshAPI {
	ttl := defaultRefreshLockTTL
	if len(lockTTL) > 0 && lockTTL[0] > 0 {
		ttl = lockTTL[0]
	}
	return &OAuthRefreshAPI{
		accountRepo: accountRepo,
		tokenCache:  tokenCache,
		lockTTL:     ttl,
	}
}

// getLocalLock 返回指定 cacheKey 的进程内互斥锁
func (api *OAuthRefreshAPI) getLocalLock(cacheKey string) *contextMutex {
	actual, _ := api.localLocks.LoadOrStore(cacheKey, newContextMutex())
	mu, ok := actual.(*contextMutex)
	if !ok {
		mu = newContextMutex()
		api.localLocks.Store(cacheKey, mu)
	}
	return mu
}

// RefreshIfNeeded 在分布式锁保护下按需刷新 OAuth token
//
// 流程:
//  1. 获取分布式锁
//  2. 从 DB 重读最新 account（防止使用过时的 refresh_token）
//  3. 二次检查是否仍需刷新
//  4. 调用 executor.Refresh() 执行平台特定刷新逻辑
//  5. 设置 _token_version + 更新 DB
//  6. 释放锁
func (api *OAuthRefreshAPI) RefreshIfNeeded(
	ctx context.Context,
	account *Account,
	executor OAuthRefreshExecutor,
	refreshWindow time.Duration,
) (*OAuthRefreshResult, error) {
	return api.refresh(ctx, account, executor, refreshWindow, false)
}

// RefreshNow 在同一套锁、DB 重读和竞争恢复保护下强制刷新 OAuth token。
// 用于上游 401 后的单次自恢复，不依赖 expires_at 是否已接近过期。
func (api *OAuthRefreshAPI) RefreshNow(
	ctx context.Context,
	account *Account,
	executor OAuthRefreshExecutor,
) (*OAuthRefreshResult, error) {
	return api.refresh(ctx, account, executor, 0, true)
}

func (api *OAuthRefreshAPI) refresh(
	ctx context.Context,
	account *Account,
	executor OAuthRefreshExecutor,
	refreshWindow time.Duration,
	force bool,
) (*OAuthRefreshResult, error) {
	if api == nil || api.accountRepo == nil {
		return nil, errors.New("oauth refresh account repository is not configured")
	}
	if account == nil {
		return nil, errors.New("oauth refresh account is nil")
	}
	if executor == nil {
		return nil, errors.New("oauth refresh executor is nil")
	}
	cacheKey := executor.CacheKey(account)
	storedOpenAI := account.IsOpenAIOAuth() && !account.IsCredentialShadow()

	// 0. 获取进程内互斥锁（防止同一进程内的并发刷新竞争）
	localMu := api.getLocalLock(cacheKey)
	if err := localMu.Lock(ctx); err != nil {
		return nil, fmt.Errorf("oauth refresh local lock: %w", err)
	}
	defer localMu.Unlock()

	// 1. 获取分布式锁
	if api.tokenCache != nil {
		acquired, lockErr := api.tokenCache.AcquireRefreshLock(ctx, cacheKey, api.lockTTL)
		if lockErr != nil {
			// Redis 错误，降级为无锁刷新（进程内互斥锁仍生效）
			slog.Warn("oauth_refresh_lock_failed_degraded",
				"account_id", account.ID,
				"cache_key", cacheKey,
				"error", lockErr,
			)
		} else if !acquired {
			// 锁被其他 worker 持有
			return &OAuthRefreshResult{LockHeld: true}, nil
		} else {
			defer api.releaseRefreshLock(ctx, cacheKey)
			if storedOpenAI {
				budget := api.lockTTL - defaultRefreshPersistenceTimeout - 2*defaultRefreshLockReleaseTimeout
				if budget <= 0 {
					return nil, errors.New("OpenAI OAuth refresh lock is too short for credential persistence")
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, budget)
				defer cancel()
			}
		}
	}

	// 2. 从 DB 重读最新 account（锁保护下，确保使用最新的 refresh_token）
	freshAccount, err := api.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		if account.IsGrokOAuth() || storedOpenAI {
			return nil, fmt.Errorf("%w: %v", errOAuthRefreshAccountRereadFailed, err)
		}
		slog.Warn("oauth_refresh_db_reread_failed",
			"account_id", account.ID,
			"error", err,
		)
		// 降级使用传入的 account
		freshAccount = account
	} else if freshAccount == nil {
		if account.IsGrokOAuth() || storedOpenAI {
			return nil, fmt.Errorf("%w: account not found", errOAuthRefreshAccountStateChanged)
		}
		freshAccount = account
	}
	if storedOpenAI && (!freshAccount.IsOpenAIOAuth() || freshAccount.IsCredentialShadow()) {
		return &OAuthRefreshResult{Account: freshAccount}, errOAuthRefreshAccountStateChanged
	}
	if (account.IsGrokOAuth() || storedOpenAI) && !executor.CanRefresh(freshAccount) {
		return &OAuthRefreshResult{Account: freshAccount}, fmt.Errorf("%w: account is no longer refreshable", errOAuthRefreshAccountStateChanged)
	}

	// 3. 二次检查是否仍需刷新（另一条路径可能已刷新）
	if force && storedOpenAI && account.GetCredential("access_token") != freshAccount.GetCredential("access_token") &&
		strings.TrimSpace(freshAccount.GetCredential("access_token")) != "" {
		if expiresAt := freshAccount.GetCredentialAsTime("expires_at"); expiresAt == nil || time.Now().Before(*expiresAt) {
			return &OAuthRefreshResult{Account: freshAccount}, nil
		}
	}
	if !force && !executor.NeedsRefresh(freshAccount, refreshWindow) {
		return &OAuthRefreshResult{
			Account: freshAccount,
		}, nil
	}

	// 4. 执行平台特定刷新逻辑
	attemptedAccount := snapshotOAuthRefreshAccount(freshAccount)
	newCredentials, refreshErr := executor.Refresh(ctx, freshAccount)
	if refreshErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// 竞争恢复：invalid_grant 可能是另一个 worker 已消费了旧 refresh_token
		// 重新读取 DB，如果 refresh_token 已更新则说明是竞争，返回成功
		if isInvalidGrantError(refreshErr) {
			recoveredAccount, recoveryErr := api.tryRecoverFromRefreshRace(ctx, attemptedAccount)
			if recoveryErr != nil {
				return nil, recoveryErr
			}
			if recoveredAccount != nil {
				slog.Info("oauth_refresh_race_recovered",
					"account_id", freshAccount.ID,
					"platform", freshAccount.Platform,
				)
				return &OAuthRefreshResult{
					Account: recoveredAccount,
				}, nil
			}
		}
		if freshAccount.IsGrokOAuth() {
			return &OAuthRefreshResult{Account: attemptedAccount}, refreshErr
		}
		return nil, refreshErr
	}

	if storedOpenAI && newCredentials != nil {
		account, applied, err := persistOpenAIRefreshedCredentials(ctx, api.accountRepo, attemptedAccount, newCredentials)
		if (applied || account != nil) && api.tokenCache != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultRefreshLockReleaseTimeout)
			_ = api.tokenCache.DeleteAccessToken(cleanupCtx, cacheKey)
			cancel()
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errOpenAIRotationPersistence, err)
		}
		result := &OAuthRefreshResult{Refreshed: applied, Account: account}
		if applied {
			result.NewCredentials = account.Credentials
		}
		return result, nil
	}

	// 5. 设置版本号 + 更新 DB
	if newCredentials != nil {
		persistenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultRefreshPersistenceTimeout)
		defer cancel()
		newCredentials["_token_version"] = time.Now().UnixMilli()
		if freshAccount.IsGrokOAuth() {
			conditionalRepo, ok := api.accountRepo.(GrokOAuthRefreshSuccessRepository)
			if !ok {
				return nil, errors.New("grok oauth refresh CAS repository is not configured")
			}
			applied, updateErr := conditionalRepo.UpdateGrokOAuthCredentialsIfUnchanged(
				persistenceCtx, freshAccount.ID, attemptedAccount.Credentials, attemptedAccount.ProxyID, newCredentials,
			)
			if updateErr != nil {
				return nil, fmt.Errorf("oauth refresh credential persistence failed: %w", updateErr)
			}
			if !applied {
				current, readErr := api.accountRepo.GetByID(persistenceCtx, freshAccount.ID)
				if readErr != nil || current == nil {
					return nil, fmt.Errorf("%w: refreshed credential state changed", errOAuthRefreshAccountStateChanged)
				}
				return &OAuthRefreshResult{Account: current}, nil
			}
			freshAccount, updateErr = api.accountRepo.GetByID(persistenceCtx, freshAccount.ID)
			if updateErr != nil || freshAccount == nil {
				return nil, fmt.Errorf("%w: refreshed account is unavailable", errOAuthRefreshAccountRereadFailed)
			}
			if api.tokenCache != nil {
				_ = api.tokenCache.DeleteAccessToken(persistenceCtx, cacheKey)
			}
		} else if updateErr := persistAccountCredentials(persistenceCtx, api.accountRepo, freshAccount, newCredentials); updateErr != nil {
			slog.Error("oauth_refresh_update_failed",
				"account_id", freshAccount.ID,
				"error", updateErr,
			)
			return nil, fmt.Errorf("oauth refresh succeeded but DB update failed: %w", updateErr)
		}
	}

	return &OAuthRefreshResult{
		Refreshed:      true,
		NewCredentials: newCredentials,
		Account:        freshAccount,
	}, nil
}

func persistOpenAIRefreshedCredentials(parent context.Context, repo AccountRepository, attempted *Account, refreshed map[string]any) (*Account, bool, error) {
	cas, ok := repo.(OpenAIOAuthRefreshSuccessRepository)
	if !ok {
		return nil, false, errors.New("OpenAI OAuth credential CAS is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), defaultRefreshPersistenceTimeout)
	defer cancel()
	version := time.Now().UnixMilli()
	if previous := attempted.GetCredentialAsInt64("_token_version"); previous >= version && previous < math.MaxInt64 {
		version = previous + 1
	}
	expected := attempted
	var lastUpdateErr error
	for i := 0; i < 3; i++ {
		replacement := cloneCredentials(expected.Credentials)
		if replacement == nil {
			replacement = make(map[string]any)
		}
		for key, value := range refreshed {
			if !reflect.DeepEqual(attempted.Credentials[key], value) {
				replacement[key] = value
			}
		}
		replacement["_token_version"] = version
		replacementAccount := Account{Credentials: replacement}
		if strings.TrimSpace(replacementAccount.GetCredential("access_token")) == "" {
			return nil, false, errors.New("OpenAI OAuth refresh produced an empty access token")
		}
		attemptCtx, endAttempt := context.WithTimeout(ctx, time.Second)
		applied, updateErr := cas.UpdateOpenAIOAuthCredentialsIfUnchanged(attemptCtx, expected.ID, expected.Credentials, expected.ProxyID, replacement)
		endAttempt()
		lastUpdateErr = updateErr
		current, readErr := repo.GetByID(ctx, attempted.ID)
		if readErr != nil || current == nil {
			if updateErr != nil && ctx.Err() == nil {
				continue
			}
			return nil, applied, errOAuthRefreshAccountRereadFailed
		}
		if !current.IsOpenAIOAuth() || current.IsCredentialShadow() {
			return nil, false, errOAuthRefreshAccountStateChanged
		}
		if applied || (updateErr != nil &&
			current.GetCredential("access_token") == replacementAccount.GetCredential("access_token") &&
			current.GetCredential("refresh_token") == replacementAccount.GetCredential("refresh_token") &&
			current.GetCredentialAsInt64("_token_version") == version) {
			return current, true, nil
		}
		if current.GetCredential("refresh_token") != attempted.GetCredential("refresh_token") ||
			current.GetCredential("access_token") != attempted.GetCredential("access_token") ||
			current.GetCredentialAsInt64("_token_version") != attempted.GetCredentialAsInt64("_token_version") {
			return current, false, nil
		}
		expected = snapshotOAuthRefreshAccount(current)
	}
	if lastUpdateErr != nil {
		return nil, false, fmt.Errorf("OpenAI OAuth credential persistence failed: %w", lastUpdateErr)
	}
	return nil, false, errOAuthRefreshAccountStateChanged
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func (api *OAuthRefreshAPI) releaseRefreshLock(parent context.Context, cacheKey string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), defaultRefreshLockReleaseTimeout)
	defer cancel()
	if err := api.tokenCache.ReleaseRefreshLock(ctx, cacheKey); err != nil {
		slog.Warn("oauth_refresh_lock_release_failed", "cache_key", cacheKey, "error", err)
	}
}

// isInvalidGrantError 检查错误是否为 invalid_grant
func isInvalidGrantError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "invalid_grant")
}

// tryRecoverFromRefreshRace 在 invalid_grant 错误后尝试竞争恢复
// 重新读取 DB，如果 refresh_token 已改变（说明另一个 worker 成功刷新），则返回更新后的 account
func (api *OAuthRefreshAPI) tryRecoverFromRefreshRace(ctx context.Context, usedAccount *Account) (*Account, error) {
	if api.accountRepo == nil {
		return nil, nil
	}
	reReadAccount, err := api.accountRepo.GetByID(ctx, usedAccount.ID)
	if usedAccount.IsOpenAIOAuth() {
		if err != nil {
			return nil, errOAuthRefreshAccountRereadFailed
		}
		if reReadAccount == nil || !reReadAccount.IsOpenAIOAuth() || reReadAccount.IsCredentialShadow() {
			return nil, errOAuthRefreshAccountStateChanged
		}
	} else if err != nil || reReadAccount == nil {
		return nil, nil
	}
	usedRT := usedAccount.GetCredential("refresh_token")
	currentRT := reReadAccount.GetCredential("refresh_token")
	if usedRT == "" || currentRT == "" {
		return nil, nil
	}
	// refresh_token 不同 → 另一个 worker 已成功刷新
	if usedRT != currentRT {
		return reReadAccount, nil
	}
	return nil, nil
}

// MergeCredentials 将旧 credentials 中不存在于新 map 的字段保留到新 map 中
func MergeCredentials(oldCreds, newCreds map[string]any) map[string]any {
	if newCreds == nil {
		newCreds = make(map[string]any)
	}
	for k, v := range oldCreds {
		if _, exists := newCreds[k]; !exists {
			newCreds[k] = v
		}
	}
	return newCreds
}

// BuildClaudeAccountCredentials 为 Claude 平台构建 OAuth credentials map
// 消除 Claude 平台没有 BuildAccountCredentials 方法的问题
func BuildClaudeAccountCredentials(tokenInfo *TokenInfo) map[string]any {
	creds := map[string]any{
		"access_token": tokenInfo.AccessToken,
		"token_type":   tokenInfo.TokenType,
		"expires_in":   strconv.FormatInt(tokenInfo.ExpiresIn, 10),
		"expires_at":   strconv.FormatInt(tokenInfo.ExpiresAt, 10),
	}
	if tokenInfo.RefreshToken != "" {
		creds["refresh_token"] = tokenInfo.RefreshToken
	}
	if tokenInfo.Scope != "" {
		creds["scope"] = tokenInfo.Scope
	}
	return creds
}
