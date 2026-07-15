package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	defaultGrokOAuthReconcilePageSize = 50
	maxGrokOAuthReconcilePageSize     = 500
	maxGrokOAuthReconcileWindow       = 24 * time.Hour

	GrokOAuthReconcileReasonMissingRefreshToken = "missing_refresh_token"
	GrokOAuthReconcileReasonMissingAccessToken  = "missing_access_token"
	GrokOAuthReconcileReasonMissingExpiry       = "missing_expiry"
	GrokOAuthReconcileReasonInvalidExpiry       = "invalid_expiry"
	GrokOAuthReconcileReasonNearExpiry          = "near_expiry"
	GrokOAuthReconcileReasonCredentialRejected  = "credential_rejected"

	GrokOAuthReconcileActionBlock   = "block_account"
	GrokOAuthReconcileActionRefresh = "refresh_credentials"

	GrokOAuthReconcileOutcomePlanned = "planned"
	GrokOAuthReconcileOutcomeApplied = "applied"
	GrokOAuthReconcileOutcomeSkipped = "skipped"
	GrokOAuthReconcileOutcomeFailed  = "failed"
)

var (
	ErrGrokOAuthReconcileMode = infraerrors.BadRequest(
		"GROK_OAUTH_RECONCILE_MODE_INVALID", "apply requires dry_run=false and apply=true",
	)
	ErrGrokOAuthReconcileCursor = infraerrors.BadRequest(
		"GROK_OAUTH_RECONCILE_CURSOR_INVALID", "after_id must be non-negative",
	)
	ErrGrokOAuthReconcileLimit = infraerrors.BadRequest(
		"GROK_OAUTH_RECONCILE_LIMIT_INVALID", "limit is outside the allowed reconciliation page range",
	)
	ErrGrokOAuthReconcileWindow = infraerrors.BadRequest(
		"GROK_OAUTH_RECONCILE_WINDOW_INVALID", "refresh_window_seconds is outside the allowed range",
	)
)

type GrokOAuthReconciler interface {
	ReconcileGrokOAuth(context.Context, GrokOAuthReconcileInput) (*GrokOAuthReconcileResult, error)
}

type GrokOAuthReconcileInput struct {
	DryRun        bool
	Apply         bool
	AfterID       int64
	Limit         int
	RefreshWindow time.Duration
}

type GrokOAuthReconcileItem struct {
	AccountID int64  `json:"account_id"`
	Reason    string `json:"reason"`
	Action    string `json:"action"`
	Outcome   string `json:"outcome"`
}

type GrokOAuthReconcileResult struct {
	DryRun       bool                     `json:"dry_run"`
	Scanned      int                      `json:"scanned"`
	Actionable   int                      `json:"actionable"`
	WouldBlock   int                      `json:"would_block"`
	WouldRefresh int                      `json:"would_refresh"`
	Blocked      int                      `json:"blocked"`
	Refreshed    int                      `json:"refreshed"`
	Skipped      int                      `json:"skipped"`
	Failed       int                      `json:"failed"`
	Items        []GrokOAuthReconcileItem `json:"items"`
	NextAfterID  int64                    `json:"next_after_id"`
	HasMore      bool                     `json:"has_more"`
}

func (s *TokenRefreshService) ReconcileGrokOAuth(
	ctx context.Context,
	input GrokOAuthReconcileInput,
) (*GrokOAuthReconcileResult, error) {
	if input.Apply && input.DryRun {
		return nil, ErrGrokOAuthReconcileMode
	}
	if input.AfterID < 0 {
		return nil, ErrGrokOAuthReconcileCursor
	}
	limit := input.Limit
	if limit == 0 {
		limit = defaultGrokOAuthReconcilePageSize
	}
	if limit < 1 || limit > maxGrokOAuthReconcilePageSize {
		return nil, ErrGrokOAuthReconcileLimit
	}
	window := input.RefreshWindow
	if window == 0 {
		window = grokTokenRefreshSkew
	}
	if window < 0 || window > maxGrokOAuthReconcileWindow {
		return nil, ErrGrokOAuthReconcileWindow
	}
	if window < grokTokenRefreshSkew {
		window = grokTokenRefreshSkew
	}
	if s == nil || s.accountRepo == nil {
		return nil, errors.New("account repository is not configured")
	}

	all, err := s.accountRepo.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	accounts := make([]Account, 0, limit+1)
	for _, account := range all {
		if account.ID > input.AfterID && account.IsGrokOAuth() {
			accounts = append(accounts, account)
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	hasMore := len(accounts) > limit
	if hasMore {
		accounts = accounts[:limit]
	}
	result := &GrokOAuthReconcileResult{
		DryRun: input.DryRun || !input.Apply, Scanned: len(accounts),
		Items: make([]GrokOAuthReconcileItem, 0, len(accounts)), HasMore: hasMore,
	}
	if hasMore && len(accounts) > 0 {
		result.NextAfterID = accounts[len(accounts)-1].ID
	}
	executor := s.grokOAuthRefreshExecutor()
	stateRepo, supportsCAS := s.accountRepo.(grokOAuthConditionalStateRepository)
	if input.Apply && (!supportsCAS || executor == nil || s.refreshAPI == nil) {
		return nil, errors.New("grok OAuth reconciliation dependencies are not configured")
	}

	for i := range accounts {
		account := &accounts[i]
		reason, action, actionable := classifyGrokOAuthReconcileAccount(account, window)
		if !actionable {
			result.Skipped++
			continue
		}
		result.Actionable++
		item := GrokOAuthReconcileItem{
			AccountID: account.ID, Reason: reason, Action: action, Outcome: GrokOAuthReconcileOutcomePlanned,
		}
		if action == GrokOAuthReconcileActionBlock {
			result.WouldBlock++
		} else {
			result.WouldRefresh++
		}
		if result.DryRun {
			result.Items = append(result.Items, item)
			continue
		}

		switch action {
		case GrokOAuthReconcileActionBlock:
			applied, applyErr := stateRepo.SetGrokOAuthErrorIfCredentialsUnchanged(
				ctx, account.ID, cloneCredentials(account.Credentials), cloneInt64Pointer(account.ProxyID),
				"Grok OAuth credentials require account action",
			)
			if applyErr != nil {
				item.Outcome = GrokOAuthReconcileOutcomeFailed
				result.Failed++
			} else if !applied {
				item.Outcome = GrokOAuthReconcileOutcomeSkipped
				result.Skipped++
			} else {
				item.Outcome = GrokOAuthReconcileOutcomeApplied
				result.Blocked++
				s.notifyAccountSchedulingBlocked(account, time.Time{}, "grok_oauth_reconciliation")
				if s.cacheInvalidator != nil {
					_ = s.cacheInvalidator.InvalidateToken(ctx, account)
				}
			}
		case GrokOAuthReconcileActionRefresh:
			refreshResult, refreshErr := s.refreshAPI.RefreshIfNeeded(ctx, account, executor, window)
			if refreshErr == nil && refreshResult != nil && refreshResult.Refreshed {
				item.Outcome = GrokOAuthReconcileOutcomeApplied
				result.Refreshed++
				s.postRefreshActions(ctx, refreshResult.Account)
			} else if refreshErr == nil {
				item.Outcome = GrokOAuthReconcileOutcomeSkipped
				result.Skipped++
			} else if isNonRetryableRefreshError(refreshErr) {
				item.Reason = GrokOAuthReconcileReasonCredentialRejected
				item.Action = GrokOAuthReconcileActionBlock
				applied, applyErr := stateRepo.SetGrokOAuthErrorIfCredentialsUnchanged(
					ctx, account.ID, cloneCredentials(account.Credentials), cloneInt64Pointer(account.ProxyID),
					"Grok OAuth credentials require account action",
				)
				if applyErr == nil && applied {
					item.Outcome = GrokOAuthReconcileOutcomeApplied
					result.Blocked++
					s.notifyAccountSchedulingBlocked(account, time.Time{}, "grok_oauth_reconciliation")
					if s.cacheInvalidator != nil {
						_ = s.cacheInvalidator.InvalidateToken(ctx, account)
					}
				} else if applyErr == nil {
					item.Outcome = GrokOAuthReconcileOutcomeSkipped
					result.Skipped++
				} else {
					item.Outcome = GrokOAuthReconcileOutcomeFailed
					result.Failed++
				}
			} else {
				item.Outcome = GrokOAuthReconcileOutcomeFailed
				result.Failed++
			}
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func (s *TokenRefreshService) grokOAuthRefreshExecutor() OAuthRefreshExecutor {
	for _, executor := range s.executors {
		if executor == nil {
			continue
		}
		probe := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "probe"}}
		if executor.CanRefresh(probe) {
			return executor
		}
	}
	return nil
}

func classifyGrokOAuthReconcileAccount(account *Account, window time.Duration) (string, string, bool) {
	if account == nil || !account.IsGrokOAuth() || account.Status != StatusActive {
		return "", "", false
	}
	if strings.TrimSpace(account.GetGrokRefreshToken()) == "" {
		accessToken := strings.TrimSpace(account.GetGrokAccessToken())
		expiresAt := account.GetCredentialAsTime("expires_at")
		// SSO conversion may issue a non-renewable token. It remains usable until
		// its explicit expiry, when the imported account auto-pauses.
		if accessToken != "" && expiresAt != nil && time.Now().Before(*expiresAt) {
			return "", "", false
		}
		return GrokOAuthReconcileReasonMissingRefreshToken, GrokOAuthReconcileActionBlock, true
	}
	if strings.TrimSpace(account.GetGrokAccessToken()) == "" {
		return GrokOAuthReconcileReasonMissingAccessToken, GrokOAuthReconcileActionRefresh, true
	}
	rawExpiry := strings.TrimSpace(account.GetCredential("expires_at"))
	if rawExpiry == "" {
		return GrokOAuthReconcileReasonMissingExpiry, GrokOAuthReconcileActionRefresh, true
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return GrokOAuthReconcileReasonInvalidExpiry, GrokOAuthReconcileActionRefresh, true
	}
	if time.Until(*expiresAt) <= window {
		return GrokOAuthReconcileReasonNearExpiry, GrokOAuthReconcileActionRefresh, true
	}
	return "", "", false
}
