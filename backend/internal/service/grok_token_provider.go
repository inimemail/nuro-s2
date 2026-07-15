package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

const (
	grokTokenCacheSkew             = 5 * time.Minute
	grokRequestRefreshTimeout      = 8 * time.Second
	grokRefreshRecoveryTimeout     = 2 * time.Second
	grokRefreshLockWaitTimeout     = 2 * time.Second
	grokRefreshLockPollInterval    = 25 * time.Millisecond
	grokTokenProviderLogComponent  = "grok_token_provider"
	grokTempUnschedulableErrorCode = "token_refresh_failed"
)

var (
	errGrokOAuthRefreshNotConfigured = errors.New("grok oauth refresh is not configured")
	errGrokOAuthAccessTokenMissing   = errors.New("grok oauth access token is missing")
	errGrokOAuthAccessTokenExpired   = errors.New("grok oauth access token is expired")
)

type GrokTokenCache = GeminiTokenCache

type grokOAuthConditionalStateRepository interface {
	SetGrokOAuthErrorIfCredentialsUnchanged(
		context.Context, int64, map[string]any, *int64, string,
	) (bool, error)
	SetGrokOAuthTempUnschedulableIfCredentialsUnchanged(
		context.Context, int64, map[string]any, *int64, time.Time, string,
	) (bool, error)
}

type GrokTokenProvider struct {
	accountRepo      AccountRepository
	tokenCache       GrokTokenCache
	refreshAPI       *OAuthRefreshAPI
	executor         OAuthRefreshExecutor
	refreshPolicy    ProviderRefreshPolicy
	tempUnschedCache TempUnschedCache
}

func NewGrokTokenProvider(
	accountRepo AccountRepository,
	tokenCache GrokTokenCache,
) *GrokTokenProvider {
	return &GrokTokenProvider{
		accountRepo:   accountRepo,
		tokenCache:    tokenCache,
		refreshPolicy: GrokProviderRefreshPolicy(),
	}
}

func (p *GrokTokenProvider) SetRefreshAPI(api *OAuthRefreshAPI, executor OAuthRefreshExecutor) {
	p.refreshAPI = api
	p.executor = executor
}

func (p *GrokTokenProvider) SetRefreshPolicy(policy ProviderRefreshPolicy) {
	p.refreshPolicy = policy
}

func (p *GrokTokenProvider) SetTempUnschedCache(cache TempUnschedCache) {
	p.tempUnschedCache = cache
}

func (p *GrokTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	return p.getAccessToken(ctx, account, isEligibleGrokOAuthRequestAccount)
}

// GetAccessTokenForProbe allows an explicit admin quota probe to inspect a
// dynamically cooled-down account while retaining all static account guards.
func (p *GrokTokenProvider) GetAccessTokenForProbe(ctx context.Context, account *Account) (string, error) {
	return p.getAccessToken(ctx, account, isEligibleGrokOAuthProbeAccount)
}

func (p *GrokTokenProvider) getAccessToken(
	ctx context.Context,
	account *Account,
	isEligible func(*Account) bool,
) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if !account.IsGrokOAuth() {
		return "", errors.New("not a grok oauth account")
	}
	selectedProxyID := cloneInt64Pointer(account.ProxyID)
	if isEligible == nil || !isEligible(account) {
		return "", errOAuthRefreshAccountStateChanged
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	accountAccessToken := strings.TrimSpace(account.GetGrokAccessToken())
	accountRefreshToken := strings.TrimSpace(account.GetGrokRefreshToken())

	cacheKey := GrokTokenCacheKey(account)
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil {
			cachedToken := strings.TrimSpace(token)
			if cachedToken != "" && cachedToken == accountAccessToken &&
				expiresAt != nil && time.Until(*expiresAt) > grokTokenRefreshSkew {
				return cachedToken, nil
			}
		}
	}

	needsRefresh := accountAccessToken == "" || expiresAt == nil || time.Until(*expiresAt) <= grokTokenRefreshSkew
	if needsRefresh && accountRefreshToken == "" {
		switch {
		case accountAccessToken == "":
			return "", errGrokOAuthAccessTokenMissing
		case expiresAt == nil || !time.Now().Before(*expiresAt):
			return "", errGrokOAuthAccessTokenExpired
		default:
			// SSO conversion can legitimately return a non-renewable access token.
			// Keep it usable until expiry; the imported account is auto-paused then.
			needsRefresh = false
		}
	}
	if needsRefresh {
		if p.refreshAPI == nil || p.executor == nil {
			return "", errGrokOAuthRefreshNotConfigured
		}
		refreshCtx, cancel := context.WithTimeout(ctx, grokRequestRefreshTimeout)
		defer cancel()
		result, err := p.refreshAPI.RefreshIfNeeded(refreshCtx, account, p.executor, grokTokenRefreshSkew)
		if err != nil {
			// A downstream cancellation is not an account failure. Returning before
			// recovery/punishment keeps abandoned requests out of scheduler health
			// and temporary-unschedulable state.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", ctxErr
			}
			attemptedAccount := account
			if result != nil && result.Account != nil {
				attemptedAccount = result.Account
			}
			// RefreshIfNeeded may return because its own timeout expired while the
			// caller is still alive. Do not reuse that already-expired context for
			// the bounded DB reread: a concurrent refresh/reauthorization that won
			// the CAS would otherwise be treated as another failed refresh.
			recoveryCtx, recoveryCancel := context.WithTimeout(ctx, grokRefreshRecoveryTimeout)
			token, _, recoveryErr := p.recoverAfterRefreshErrorWithEligibility(
				recoveryCtx, attemptedAccount, selectedProxyID, isEligible,
			)
			recoveryCancel()
			if recoveryErr != nil {
				return "", recoveryErr
			}
			if token != "" {
				return token, nil
			}
			// Punish only the exact credentials used by the failed provider call.
			// A concurrent admin reauthorization must win the repository CAS.
			p.markTempUnschedulable(attemptedAccount, err)
			if p.refreshPolicy.OnRefreshError == ProviderRefreshErrorReturn {
				return "", err
			}
		} else if result != nil && result.LockHeld {
			if p.refreshPolicy.OnLockHeld == ProviderLockHeldWaitForCache {
				return p.waitForRefreshedTokenWithEligibility(
					refreshCtx, account, selectedProxyID, cacheKey, isEligible,
				)
			}
			if expiresAt == nil || !time.Now().Before(*expiresAt) {
				return "", errGrokOAuthAccessTokenExpired
			}
		} else if result != nil && result.Account != nil {
			if !isEligible(result.Account) ||
				!sameInt64Pointer(result.Account.ProxyID, selectedProxyID) {
				return "", errOAuthRefreshAccountStateChanged
			}
			account = result.Account
			expiresAt = account.GetCredentialAsTime("expires_at")
		}
	}

	accessToken := strings.TrimSpace(account.GetGrokAccessToken())
	if accessToken == "" {
		return "", errGrokOAuthAccessTokenMissing
	}
	if expiresAt == nil || !time.Now().Before(*expiresAt) {
		return "", errGrokOAuthAccessTokenExpired
	}

	if p.tokenCache != nil {
		latestAccount, isStale := CheckTokenVersion(ctx, account, p.accountRepo)
		if isStale && latestAccount != nil {
			if !isEligible(latestAccount) ||
				!sameInt64Pointer(latestAccount.ProxyID, selectedProxyID) {
				return "", errOAuthRefreshAccountStateChanged
			}
			accessToken = strings.TrimSpace(latestAccount.GetGrokAccessToken())
			latestExpiry := latestAccount.GetCredentialAsTime("expires_at")
			if accessToken == "" {
				return "", errGrokOAuthAccessTokenMissing
			}
			if latestExpiry == nil || !time.Now().Before(*latestExpiry) {
				return "", errGrokOAuthAccessTokenExpired
			}
			expiresAt = latestExpiry
		}
		ttl := time.Until(*expiresAt)
		if ttl > grokTokenCacheSkew {
			ttl -= grokTokenCacheSkew
		}
		if ttl > 0 {
			_ = p.tokenCache.SetAccessToken(ctx, cacheKey, accessToken, ttl)
		}
	}
	return accessToken, nil
}

func (p *GrokTokenProvider) recoverAfterRefreshError(
	ctx context.Context,
	used *Account,
	selectedProxyID *int64,
) (string, *Account, error) {
	return p.recoverAfterRefreshErrorWithEligibility(ctx, used, selectedProxyID, isEligibleGrokOAuthRequestAccount)
}

func (p *GrokTokenProvider) recoverAfterRefreshErrorWithEligibility(
	ctx context.Context,
	used *Account,
	selectedProxyID *int64,
	isEligible func(*Account) bool,
) (string, *Account, error) {
	if p == nil || p.accountRepo == nil || used == nil {
		return "", nil, nil
	}
	latest, err := p.accountRepo.GetByID(ctx, used.ID)
	if err != nil || latest == nil {
		return "", nil, nil
	}
	if isEligible == nil || !isEligible(latest) ||
		!sameInt64Pointer(latest.ProxyID, selectedProxyID) {
		return "", latest, errOAuthRefreshAccountStateChanged
	}
	changed := latest.GetCredential("refresh_token") != used.GetCredential("refresh_token") ||
		latest.GetCredentialAsInt64("_token_version") > used.GetCredentialAsInt64("_token_version")
	token := strings.TrimSpace(latest.GetGrokAccessToken())
	expiresAt := latest.GetCredentialAsTime("expires_at")
	if changed && token != "" && expiresAt != nil && time.Now().Before(*expiresAt) {
		return token, latest, nil
	}
	return "", latest, nil
}

func (p *GrokTokenProvider) waitForRefreshedToken(
	ctx context.Context,
	account *Account,
	selectedProxyID *int64,
	cacheKey string,
) (string, error) {
	return p.waitForRefreshedTokenWithEligibility(
		ctx, account, selectedProxyID, cacheKey, isEligibleGrokOAuthRequestAccount,
	)
}

func (p *GrokTokenProvider) waitForRefreshedTokenWithEligibility(
	ctx context.Context,
	account *Account,
	selectedProxyID *int64,
	cacheKey string,
	isEligible func(*Account) bool,
) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, grokRefreshLockWaitTimeout)
	defer cancel()
	initialToken := strings.TrimSpace(account.GetGrokAccessToken())
	initialVersion := account.GetCredentialAsInt64("_token_version")
	ticker := time.NewTicker(grokRefreshLockPollInterval)
	defer ticker.Stop()
	var lastReadErr error
	for {
		latest, err := p.accountRepo.GetByID(waitCtx, account.ID)
		if err != nil {
			lastReadErr = err
		} else if latest == nil {
			return "", errOAuthRefreshAccountStateChanged
		} else {
			if isEligible == nil || !isEligible(latest) ||
				!sameInt64Pointer(latest.ProxyID, selectedProxyID) {
				return "", errOAuthRefreshAccountStateChanged
			}
			token := strings.TrimSpace(latest.GetGrokAccessToken())
			version := latest.GetCredentialAsInt64("_token_version")
			expiresAt := latest.GetCredentialAsTime("expires_at")
			changed := token != initialToken || (version > 0 && version > initialVersion)
			if token != "" && changed && expiresAt != nil && time.Now().Before(*expiresAt) {
				if p.tokenCache != nil {
					ttl := time.Until(*expiresAt)
					if ttl > grokTokenCacheSkew {
						ttl -= grokTokenCacheSkew
					}
					if ttl > 0 {
						_ = p.tokenCache.SetAccessToken(waitCtx, cacheKey, token, ttl)
					}
				}
				return token, nil
			}
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if lastReadErr != nil {
				return "", fmt.Errorf("%w: %v", errOAuthRefreshAccountRereadFailed, lastReadErr)
			}
			return "", errOAuthRefreshAccountStateChanged
		case <-ticker.C:
		}
	}
}

func isEligibleGrokOAuthRequestAccount(account *Account) bool {
	return account != nil && account.IsGrokOAuth() && account.IsSchedulable()
}

func isEligibleGrokOAuthProbeAccount(account *Account) bool {
	if account == nil || !account.IsGrokOAuth() || !account.IsActive() || !account.Schedulable {
		return false
	}
	return !account.AutoPauseOnExpired || account.ExpiresAt == nil || time.Now().Before(*account.ExpiresAt)
}

func sameInt64Pointer(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (p *GrokTokenProvider) markTempUnschedulable(account *Account, refreshErr error) {
	if p == nil || p.accountRepo == nil || account == nil {
		return
	}
	now := time.Now()
	until := now.Add(tokenRefreshTempUnschedDuration)
	redactedErr := "unknown error"
	if refreshErr != nil {
		redactedErr = logredact.RedactText(refreshErr.Error())
	}
	if isNonRetryableRefreshError(refreshErr) {
		repo, ok := p.accountRepo.(grokOAuthConditionalStateRepository)
		if !ok {
			slog.Warn(grokTokenProviderLogComponent+".conditional_state_repository_missing", "account_id", account.ID)
			return
		}
		applied, err := repo.SetGrokOAuthErrorIfCredentialsUnchanged(
			context.Background(), account.ID, cloneCredentials(account.Credentials),
			cloneInt64Pointer(account.ProxyID), "token refresh failed (non-retryable): "+redactedErr,
		)
		if err != nil {
			slog.Warn(grokTokenProviderLogComponent+".set_error_status_failed", "account_id", account.ID, "error", err)
		} else if applied && p.tokenCache != nil {
			if cacheErr := p.tokenCache.DeleteAccessToken(context.Background(), GrokTokenCacheKey(account)); cacheErr != nil {
				slog.Warn(grokTokenProviderLogComponent+".cache_delete_failed", "account_id", account.ID, "error", cacheErr)
			}
		}
		return
	}
	reason := "grok token refresh failed on request path: " + redactedErr
	bgCtx := context.Background()
	repo, ok := p.accountRepo.(grokOAuthConditionalStateRepository)
	if !ok {
		slog.Warn(grokTokenProviderLogComponent+".conditional_state_repository_missing", "account_id", account.ID)
		return
	}
	applied, err := repo.SetGrokOAuthTempUnschedulableIfCredentialsUnchanged(
		bgCtx, account.ID, cloneCredentials(account.Credentials),
		cloneInt64Pointer(account.ProxyID), until, reason,
	)
	if err != nil {
		slog.Warn(grokTokenProviderLogComponent+".set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	if !applied {
		return
	}
	if p.tempUnschedCache != nil {
		state := &TempUnschedState{
			UntilUnix:       until.Unix(),
			TriggeredAtUnix: now.Unix(),
			ErrorMessage:    grokTempUnschedulableErrorCode + ": " + reason,
		}
		if err := p.tempUnschedCache.SetTempUnsched(bgCtx, account.ID, state); err != nil {
			slog.Warn(grokTokenProviderLogComponent+".temp_unsched_cache_set_failed", "account_id", account.ID, "error", err)
		}
	}
}

func (p *GrokTokenProvider) InvalidateToken(ctx context.Context, account *Account) error {
	if p == nil || p.tokenCache == nil || account == nil {
		return nil
	}
	return p.tokenCache.DeleteAccessToken(ctx, GrokTokenCacheKey(account))
}

func GrokTokenCacheKey(account *Account) string {
	if account == nil {
		return "grok:account:0"
	}
	return "grok:account:" + strconv.FormatInt(account.ID, 10)
}
