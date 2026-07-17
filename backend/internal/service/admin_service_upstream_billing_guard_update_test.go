//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type updateAccountGuardRepoStub struct {
	mockAccountRepoForGemini
	account     *Account
	updateCalls int
}

func (r *updateAccountGuardRepoStub) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *updateAccountGuardRepoStub) Update(_ context.Context, account *Account) error {
	r.updateCalls++
	r.account = account
	return nil
}

func TestUpdateAccountEnablesGuardAndAutomaticProbeTogether(t *testing.T) {
	limit := 1.5
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:            359,
		Platform:      PlatformOpenAI,
		Type:          AccountTypeAPIKey,
		Status:        StatusActive,
		Extra:         map[string]any{UpstreamBillingProbeEnabledExtraKey: false},
		AccountGroups: []AccountGroup{{GroupID: 7, UpstreamBillingGuardMaxMultiplier: &limit}},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	enabled := true

	updated, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{
		UpstreamBillingGuardEnabled: &enabled,
	})

	require.NoError(t, err)
	require.Equal(t, 1, repo.updateCalls)
	require.True(t, updated.UpstreamBillingGuardEnabled)
	require.Equal(t, true, updated.Extra[UpstreamBillingProbeEnabledExtraKey])
}

func TestUpdateAccountRejectsGuardWithoutConfiguredOpenAIGroup(t *testing.T) {
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:       359,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Extra:    map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	enabled := true

	_, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{
		UpstreamBillingGuardEnabled: &enabled,
	})

	require.ErrorIs(t, err, ErrUpstreamBillingGuardRequiresGroupLimit)
	require.Zero(t, repo.updateCalls)
}

func TestUpdateAccountRejectsGuardForFinalOAuthType(t *testing.T) {
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:       359,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Extra:    map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	enabled := true

	_, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{
		Type:                        AccountTypeOAuth,
		UpstreamBillingGuardEnabled: &enabled,
	})

	require.ErrorIs(t, err, ErrUpstreamBillingProbeAccountInvalid)
	require.Zero(t, repo.updateCalls)
}

func TestUpdateAccountClearsGuardWhenLeavingAPIKeyType(t *testing.T) {
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:                          359,
		Platform:                    PlatformOpenAI,
		Type:                        AccountTypeAPIKey,
		Status:                      StatusActive,
		Extra:                       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardEnabled: true,
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	updated, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{Type: AccountTypeOAuth})

	require.NoError(t, err)
	require.Equal(t, 1, repo.updateCalls)
	require.Equal(t, AccountTypeOAuth, updated.Type)
	require.False(t, updated.UpstreamBillingGuardEnabled)
}

func TestUpdateAccountDisablesGuardWithoutDisablingObservation(t *testing.T) {
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:                          359,
		Platform:                    PlatformOpenAI,
		Type:                        AccountTypeAPIKey,
		Status:                      StatusActive,
		Extra:                       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardEnabled: true,
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	enabled := false

	updated, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{
		UpstreamBillingGuardEnabled: &enabled,
	})

	require.NoError(t, err)
	require.False(t, updated.UpstreamBillingGuardEnabled)
	require.Equal(t, true, updated.Extra[UpstreamBillingProbeEnabledExtraKey])
}

func TestUpdateAccountClearsGuardWhenLastProtectedGroupIsRemoved(t *testing.T) {
	limit := 1.5
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:                          359,
		Platform:                    PlatformOpenAI,
		Type:                        AccountTypeAPIKey,
		Status:                      StatusActive,
		Extra:                       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardEnabled: true,
		AccountGroups:               []AccountGroup{{GroupID: 7, UpstreamBillingGuardMaxMultiplier: &limit}},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	groupIDs := []int64{}

	updated, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{GroupIDs: &groupIDs})

	require.NoError(t, err)
	require.Equal(t, 1, repo.updateCalls)
	require.False(t, updated.UpstreamBillingGuardEnabled)
}

func TestUpdateAccountRejectsDisablingProbeWhileGuardRemainsEnabled(t *testing.T) {
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:                          359,
		Platform:                    PlatformOpenAI,
		Type:                        AccountTypeAPIKey,
		Status:                      StatusActive,
		Extra:                       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardEnabled: true,
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{
		Extra: map[string]any{UpstreamBillingProbeEnabledExtraKey: false},
	})

	require.ErrorIs(t, err, ErrUpstreamBillingProbeRequiredByGuard)
	require.Zero(t, repo.updateCalls)
}
