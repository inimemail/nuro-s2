//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type updateAccountGuardRepoStub struct {
	mockAccountRepoForGemini
	account          *Account
	atomicCalls      int
	guardLimits      *map[int64]float64
	propagateShadows bool
}

func (r *updateAccountGuardRepoStub) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *updateAccountGuardRepoStub) UpdateAccountWithGroupConfig(
	_ context.Context,
	account *Account,
	_ *[]int64,
	guardLimits *map[int64]float64,
	propagateShadows bool,
) error {
	r.atomicCalls++
	r.account = account
	r.guardLimits = guardLimits
	r.propagateShadows = propagateShadows
	return nil
}

func TestUpdateAccountRejectsGuardLimitsForFinalOAuthType(t *testing.T) {
	limit := 1.0
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:       359,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Extra:    map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		GroupIDs: []int64{10},
		AccountGroups: []AccountGroup{{
			AccountID:                         359,
			GroupID:                           10,
			UpstreamBillingGuardMaxMultiplier: &limit,
		}},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	requested := map[int64]float64{10: 2}

	_, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{
		Type:                            AccountTypeOAuth,
		UpstreamBillingGuardGroupLimits: &requested,
	})

	require.ErrorIs(t, err, ErrUpstreamBillingProbeAccountInvalid)
	require.Zero(t, repo.atomicCalls)
}

func TestUpdateAccountClearsExistingGuardLimitsWhenLeavingAPIKeyType(t *testing.T) {
	limit := 1.0
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:       359,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Extra:    map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		GroupIDs: []int64{10},
		AccountGroups: []AccountGroup{{
			AccountID:                         359,
			GroupID:                           10,
			UpstreamBillingGuardMaxMultiplier: &limit,
		}},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	updated, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{Type: AccountTypeOAuth})

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, AccountTypeOAuth, updated.Type)
	require.Equal(t, 1, repo.atomicCalls)
	require.NotNil(t, repo.guardLimits)
	require.Empty(t, *repo.guardLimits)
	require.False(t, repo.propagateShadows)
}

func TestUpdateAccountRejectsInvalidGuardLimitAsBadRequest(t *testing.T) {
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:       359,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Extra:    map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		GroupIDs: []int64{10},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	requested := map[int64]float64{0: 1}

	_, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{
		UpstreamBillingGuardGroupLimits: &requested,
	})

	require.Error(t, err)
	statusCode, status := infraerrors.ToHTTP(err)
	require.Equal(t, http.StatusBadRequest, statusCode)
	require.Equal(t, "INVALID_UPSTREAM_BILLING_GUARD_GROUP_LIMIT", status.Reason)
	require.Zero(t, repo.atomicCalls)
}

func TestUpdateAccountRejectsUnboundGuardGroupAsBadRequest(t *testing.T) {
	repo := &updateAccountGuardRepoStub{account: &Account{
		ID:       359,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Extra:    map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		GroupIDs: []int64{10},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	requested := map[int64]float64{20: 1}

	_, err := svc.UpdateAccount(context.Background(), 359, &UpdateAccountInput{
		UpstreamBillingGuardGroupLimits: &requested,
	})

	require.Error(t, err)
	statusCode, status := infraerrors.ToHTTP(err)
	require.Equal(t, http.StatusBadRequest, statusCode)
	require.Equal(t, "UPSTREAM_BILLING_GUARD_GROUP_NOT_BOUND", status.Reason)
	require.Zero(t, repo.atomicCalls)
}
