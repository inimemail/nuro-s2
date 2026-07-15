//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type grokReconcileLocalRepo struct {
	mockAccountRepoForGemini
	accounts      []Account
	blockedIDs    []int64
	credentialIDs []int64
}

func (r *grokReconcileLocalRepo) ListActive(context.Context) ([]Account, error) {
	accounts := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Status == StatusActive {
			accounts = append(accounts, *cloneGrokRefreshTestAccount(&account))
		}
	}
	return accounts, nil
}

func (r *grokReconcileLocalRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			return cloneGrokRefreshTestAccount(&r.accounts[i]), nil
		}
	}
	return nil, ErrAccountNotFound
}

func (r *grokReconcileLocalRepo) UpdateGrokOAuthCredentialsIfUnchanged(
	_ context.Context,
	id int64,
	expectedCredentials map[string]any,
	expectedProxyID *int64,
	credentials map[string]any,
) (bool, error) {
	for i := range r.accounts {
		account := &r.accounts[i]
		if account.ID != id || !reflect.DeepEqual(account.Credentials, expectedCredentials) ||
			!sameInt64Pointer(account.ProxyID, expectedProxyID) {
			continue
		}
		account.Credentials = cloneCredentials(credentials)
		r.credentialIDs = append(r.credentialIDs, id)
		return true, nil
	}
	return false, nil
}

func (r *grokReconcileLocalRepo) SetGrokOAuthErrorIfCredentialsUnchanged(
	_ context.Context,
	id int64,
	expectedCredentials map[string]any,
	expectedProxyID *int64,
	message string,
) (bool, error) {
	for i := range r.accounts {
		account := &r.accounts[i]
		if account.ID != id || account.Status != StatusActive || !account.Schedulable ||
			!reflect.DeepEqual(account.Credentials, expectedCredentials) ||
			!sameInt64Pointer(account.ProxyID, expectedProxyID) {
			continue
		}
		account.Status = StatusError
		account.Schedulable = false
		account.ErrorMessage = message
		r.blockedIDs = append(r.blockedIDs, id)
		return true, nil
	}
	return false, nil
}

func (r *grokReconcileLocalRepo) SetGrokOAuthTempUnschedulableIfCredentialsUnchanged(
	context.Context, int64, map[string]any, *int64, time.Time, string,
) (bool, error) {
	return false, nil
}

type grokReconcileInvalidator struct{ ids []int64 }

func (i *grokReconcileInvalidator) InvalidateToken(_ context.Context, account *Account) error {
	if account != nil {
		i.ids = append(i.ids, account.ID)
	}
	return nil
}

func grokReconcileLocalFixtures() []Account {
	now := time.Now().UTC()
	return []Account{
		{
			ID: 1, Platform: PlatformGrok, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: true,
			Credentials: map[string]any{"access_token": "missing-refresh-secret"},
		},
		{
			ID: 2, Platform: PlatformGrok, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: true,
			Credentials: map[string]any{
				"refresh_token": "refresh-secret",
				"expires_at":    now.Add(10 * time.Minute).Format(time.RFC3339),
			},
		},
		{
			ID: 3, Platform: PlatformGrok, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: true,
			Credentials: map[string]any{
				"access_token":  "healthy-access-secret",
				"refresh_token": "healthy-refresh-secret",
				"expires_at":    now.Add(4 * time.Hour).Format(time.RFC3339),
			},
		},
		{
			ID: 4, Platform: PlatformGrok, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true,
			Credentials: map[string]any{"api_key": "api-key-secret"},
		},
	}
}

func TestReconcileGrokOAuthDryRunIsMetadataOnly(t *testing.T) {
	repo := &grokReconcileLocalRepo{accounts: grokReconcileLocalFixtures()}
	svc := &TokenRefreshService{accountRepo: repo}

	result, err := svc.ReconcileGrokOAuth(context.Background(), GrokOAuthReconcileInput{})

	require.NoError(t, err)
	require.True(t, result.DryRun)
	require.Equal(t, 3, result.Scanned)
	require.Equal(t, 2, result.Actionable)
	require.Equal(t, 1, result.WouldBlock)
	require.Equal(t, 1, result.WouldRefresh)
	require.Empty(t, repo.blockedIDs)
	require.Empty(t, repo.credentialIDs)
	payload, err := json.Marshal(result)
	require.NoError(t, err)
	for _, secret := range []string{"missing-refresh-secret", "refresh-secret", "healthy-access-secret", "api-key-secret", `"credentials":`} {
		require.NotContains(t, string(payload), secret)
	}
}

func TestReconcileGrokOAuthApplyIsIdempotent(t *testing.T) {
	repo := &grokReconcileLocalRepo{accounts: grokReconcileLocalFixtures()}
	invalidator := &grokReconcileInvalidator{}
	executor := &grokRefreshCASExecutorStub{credentials: map[string]any{
		"access_token":  "rotated-access",
		"refresh_token": "rotated-refresh",
		"expires_at":    time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339),
	}}
	svc := &TokenRefreshService{
		accountRepo: repo, executors: []OAuthRefreshExecutor{executor},
		refreshAPI: NewOAuthRefreshAPI(repo, nil), cacheInvalidator: invalidator,
	}

	first, err := svc.ReconcileGrokOAuth(context.Background(), GrokOAuthReconcileInput{Apply: true, Limit: 50})
	require.NoError(t, err)
	require.False(t, first.DryRun)
	require.Equal(t, 1, first.Blocked)
	require.Equal(t, 1, first.Refreshed)
	require.Zero(t, first.Failed)
	require.Equal(t, []int64{1}, repo.blockedIDs)
	require.Equal(t, []int64{2}, repo.credentialIDs)
	sort.Slice(invalidator.ids, func(i, j int) bool { return invalidator.ids[i] < invalidator.ids[j] })
	require.Equal(t, []int64{1, 2}, invalidator.ids)

	second, err := svc.ReconcileGrokOAuth(context.Background(), GrokOAuthReconcileInput{Apply: true, Limit: 50})
	require.NoError(t, err)
	require.Zero(t, second.Actionable)
	require.Equal(t, []int64{1}, repo.blockedIDs)
	require.Equal(t, []int64{2}, repo.credentialIDs)
}

func TestReconcileGrokOAuthNonRetryableRefreshBlocksRuntimeAndInvalidatesToken(t *testing.T) {
	repo := &grokReconcileLocalRepo{accounts: grokReconcileLocalFixtures()}
	invalidator := &grokReconcileInvalidator{}
	blocker := &runtimeBlockRecorder{}
	executor := &grokRefreshCASExecutorStub{err: errors.New("invalid_grant")}
	svc := &TokenRefreshService{
		accountRepo: repo, executors: []OAuthRefreshExecutor{executor},
		refreshAPI: NewOAuthRefreshAPI(repo, nil), cacheInvalidator: invalidator,
		runtimeBlocker: blocker,
	}

	result, err := svc.ReconcileGrokOAuth(context.Background(), GrokOAuthReconcileInput{Apply: true, Limit: 50})

	require.NoError(t, err)
	require.Equal(t, 2, result.Blocked)
	require.Zero(t, result.Refreshed)
	sort.Slice(invalidator.ids, func(i, j int) bool { return invalidator.ids[i] < invalidator.ids[j] })
	require.Equal(t, []int64{1, 2}, invalidator.ids)
	require.Len(t, blocker.accounts, 2)
	require.Equal(t, []string{"grok_oauth_reconciliation", "grok_oauth_reconciliation"}, blocker.reasons)
}

func TestClassifyGrokOAuthReconcileKeepsUnexpiredNonRenewableSSOToken(t *testing.T) {
	account := newGrokRefreshCASTestAccount()
	delete(account.Credentials, "refresh_token")
	account.Credentials["expires_at"] = time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)

	reason, action, actionable := classifyGrokOAuthReconcileAccount(account, grokTokenRefreshSkew)

	require.False(t, actionable)
	require.Empty(t, reason)
	require.Empty(t, action)
}
