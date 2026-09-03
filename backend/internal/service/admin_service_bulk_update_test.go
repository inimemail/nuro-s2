//go:build unit

package service

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type accountRepoStubForBulkUpdate struct {
	accountRepoStub
	bulkUpdateErr     error
	bulkUpdateIDs     []int64
	bulkUpdatePayload AccountBulkUpdate
	bulkUpdateCalls   []bulkUpdateCall
	bindGroupErrByID  map[int64]error
	bindGroupsCalls   []int64
	getByIDsAccounts  []*Account
	getByIDsErr       error
	getByIDsCalled    bool
	getByIDsIDs       []int64
	getByIDAccounts   map[int64]*Account
	getByIDErrByID    map[int64]error
	getByIDCalled     []int64
	listByGroupData   map[int64][]Account
	listByGroupErr    map[int64]error
	listData          []Account
	listResult        *pagination.PaginationResult
	listErr           error
	listCalled        bool
	lastListParams    pagination.PaginationParams
	lastListFilters   struct {
		platform    string
		accountType string
		status      string
		search      string
		groupID     int64
		privacyMode string
	}
	shadowsByParent map[int64][]*Account
	updateCalls     []*Account
	createdAccount  *Account
	updateExtraIDs  []int64
	updateExtra     map[string]any
}

type bulkUpdateCall struct {
	IDs     []int64
	Updates AccountBulkUpdate
}

type failingCrossReplicaRuntimeBlocker struct {
	runtimeBlockRecorder
	clears []int64
}

func (b *failingCrossReplicaRuntimeBlocker) ClearAccountSchedulingBlockAcrossReplicas(_ context.Context, accountID int64) error {
	b.clears = append(b.clears, accountID)
	return errors.New("runtime clear unavailable")
}

func (s *accountRepoStubForBulkUpdate) BulkUpdate(_ context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	s.bulkUpdateIDs = append([]int64{}, ids...)
	s.bulkUpdatePayload = updates
	s.bulkUpdateCalls = append(s.bulkUpdateCalls, bulkUpdateCall{
		IDs:     append([]int64{}, ids...),
		Updates: updates,
	})
	if s.bulkUpdateErr != nil {
		return 0, s.bulkUpdateErr
	}
	return int64(len(ids)), nil
}

func (s *accountRepoStubForBulkUpdate) ListShadowsByParent(_ context.Context, parentID int64) ([]*Account, error) {
	if s.shadowsByParent == nil {
		return nil, nil
	}
	return s.shadowsByParent[parentID], nil
}

func (s *accountRepoStubForBulkUpdate) Update(_ context.Context, account *Account) error {
	copied := *account
	s.updateCalls = append(s.updateCalls, &copied)
	return nil
}

func (s *accountRepoStubForBulkUpdate) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	s.updateExtraIDs = append(s.updateExtraIDs, id)
	s.updateExtra = maps.Clone(updates)
	return nil
}

func (s *accountRepoStubForBulkUpdate) Create(_ context.Context, account *Account) error {
	copied := *account
	copied.Extra = maps.Clone(account.Extra)
	s.createdAccount = &copied
	return nil
}

func TestAdminServiceCreateAccount_AcceptsLegacyProbeFlagInExtra(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "legacy-probe",
		Platform:             PlatformAnthropic,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"api_key": "test-key"},
		Extra:                map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		SkipDefaultGroupBind: true,
	})

	require.NoError(t, err)
	require.NotNil(t, repo.createdAccount)
	require.Equal(t, true, created.Extra[UpstreamBillingProbeEnabledExtraKey])
}

func TestAdminServiceCreateAccount_RejectsConflictingProbeFlags(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}
	enabled := false

	created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "conflicting-probe",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"api_key": "test-key"},
		Extra:                map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		ProbeEnabled:         &enabled,
		SkipDefaultGroupBind: true,
	})

	require.Nil(t, created)
	require.Error(t, err)
	require.Nil(t, repo.createdAccount)
}

func TestAdminServiceCreateAccount_FirstTokenTimeoutRejectsUnsupportedTargets(t *testing.T) {
	tests := []struct {
		name  string
		input *CreateAccountInput
	}{
		{name: "other platform", input: &CreateAccountInput{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}},
		{name: "image pool", input: &CreateAccountInput{
			Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"pool_mode": true, "image_pool_mode": true},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.input.Extra = map[string]any{
				openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
					map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
				},
			}
			tt.input.SkipDefaultGroupBind = true
			repo := &accountRepoStubForBulkUpdate{}
			svc := &adminServiceImpl{accountRepo: repo}
			created, err := svc.CreateAccount(context.Background(), tt.input)
			require.Error(t, err)
			require.Nil(t, created)
			require.Nil(t, repo.createdAccount)
		})
	}
}

func TestAdminServiceCreateAccount_FirstTokenTimeoutAcceptsIndependentOAuth(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}
	created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:     "openai-oauth",
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
				map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			},
		},
		SkipDefaultGroupBind: true,
	})
	require.NoError(t, err)
	require.NotNil(t, created)
	require.Len(t, created.Extra[openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey], 1)
	require.Equal(t, 800, created.Extra[openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey])
	require.Equal(t, 5000, created.Extra[openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey])
}

func TestAdminServiceCreateAccount_FirstTokenTimeoutAcceptsIndependentAPIKey(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}
	created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:        "openai-key",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test-key"},
		Extra: map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
				map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			},
		},
		SkipDefaultGroupBind: true,
	})
	require.NoError(t, err)
	require.NotNil(t, created)
	require.Len(t, created.Extra[openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey], 1)
}

func TestAdminServiceUpdateAccount_FirstTokenTimeoutRejectsUnsupportedTarget(t *testing.T) {
	parentID := int64(3)
	tests := []struct {
		name    string
		account *Account
	}{
		{name: "other platform", account: &Account{ID: 7, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}},
		{name: "image pool", account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "image_pool_mode": true}}},
		{name: "credential shadow", account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, ParentAccountID: &parentID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.account.Extra = map[string]any{
				openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey: 800,
			}
			repo := &accountRepoStubForBulkUpdate{getByIDAccounts: map[int64]*Account{7: tt.account}}
			svc := &adminServiceImpl{accountRepo: repo}
			updated, err := svc.UpdateAccount(context.Background(), 7, &UpdateAccountInput{Extra: tt.account.Extra})
			require.Error(t, err)
			require.Nil(t, updated)
			require.Empty(t, repo.updateCalls)
		})
	}
}

func TestAdminServiceUpdateAccount_FirstTokenTimeoutRejectsFinalImagePoolIdentity(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDAccounts: map[int64]*Account{
		7: {ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "old"}},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.UpdateAccount(context.Background(), 7, &UpdateAccountInput{
		Credentials: map[string]any{"pool_mode": true, "image_pool_mode": true},
		Extra: map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey: 800,
		},
	})
	require.Error(t, err)
	require.Nil(t, updated)
	require.Empty(t, repo.updateCalls)
}

func TestAdminServiceUpdateAccountExtra_FirstTokenTimeoutRejectsUnsupportedTarget(t *testing.T) {
	parentID := int64(3)
	tests := []struct {
		name    string
		account *Account
	}{
		{name: "other platform", account: &Account{ID: 7, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}},
		{name: "image pool", account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "image_pool_mode": true}}},
		{name: "credential shadow", account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, ParentAccountID: &parentID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &accountRepoStubForBulkUpdate{getByIDAccounts: map[int64]*Account{7: tt.account}}
			svc := &adminServiceImpl{accountRepo: repo}
			err := svc.UpdateAccountExtra(context.Background(), 7, map[string]any{
				openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey: 800,
			})
			require.Error(t, err)
		})
	}
}

func TestAdminServiceUpdateAccountExtra_PreservesDeepSeekResponsesWhenRemovingLegacyURLs(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDAccounts: map[int64]*Account{
		7: {
			ID:       7,
			Platform: PlatformDeepSeek,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"api_protocol":  APIProtocolResponses,
				"api_base_urls": map[string]any{"responses": "https://legacy.example"},
			},
			Extra: map[string]any{
				cnAPIProtocolExtraKey: APIProtocolResponses,
				cnAPIBaseURLsExtraKey: map[string]any{"responses": "https://legacy.example"},
			},
		},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	err := svc.UpdateAccountExtra(context.Background(), 7, map[string]any{
		cnAPIBaseURLsExtraKey: map[string]any{},
	})

	require.NoError(t, err)
	require.Equal(t, []int64{7}, repo.updateExtraIDs)
	require.Equal(t, APIProtocolResponses, repo.updateExtra[cnAPIProtocolExtraKey])
	require.Nil(t, repo.updateExtra[cnAPIBaseURLsExtraKey])
}

func TestAdminServiceBulkUpdateAccounts_AcceptsLegacyProbeFlagInExtra(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{
		getByIDsAccounts: []*Account{{ID: 7, Platform: PlatformGemini, Type: AccountTypeAPIKey}},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{7},
		Extra: map[string]any{
			UpstreamBillingProbeEnabledExtraKey: true,
			"feature":                           true,
		},
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Equal(t, true, repo.bulkUpdatePayload.Extra[UpstreamBillingProbeEnabledExtraKey])
	require.Equal(t, true, repo.bulkUpdatePayload.Extra["feature"])
}

func TestAdminServiceBulkUpdateAccounts_RejectsConflictingProbeFlags(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}
	enabled := false

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:   []int64{7},
		Extra:        map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		ProbeEnabled: &enabled,
	})

	require.Nil(t, result)
	require.Error(t, err)
	require.Empty(t, repo.bulkUpdateCalls)
}

func TestAdminServiceBulkUpdateAccounts_ForwardsExtraRemoveKeys(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	input := &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2},
		ExtraRemoveKeys: []string{
			"codex_image_generation_bridge",
			"codex_image_generation_bridge_enabled",
			"codex_image_generation_explicit_tool_policy",
		},
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.Equal(t, []int64{1, 2}, repo.bulkUpdateIDs)
	require.Equal(t, []string{
		"codex_image_generation_bridge",
		"codex_image_generation_bridge_enabled",
		"codex_image_generation_explicit_tool_policy",
	}, repo.bulkUpdatePayload.ExtraRemoveKeys)
}

func TestAdminServiceBulkUpdateAccounts_ForwardsZhipuCredentialChanges(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2},
		Credentials: map[string]any{
			"zhipu_organization": "org-updated",
			"zhipu_project":      "project-updated",
		},
		CredentialsRemoveKeys: []string{"zhipu_project"},
	})

	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.Equal(t, "org-updated", repo.bulkUpdatePayload.Credentials["zhipu_organization"])
	require.Equal(t, "project-updated", repo.bulkUpdatePayload.Credentials["zhipu_project"])
	require.Equal(t, []string{"zhipu_project"}, repo.bulkUpdatePayload.CredentialsRemoveKeys)
}

func TestAdminServiceBulkUpdateAccounts_NormalizesCNProtocolPerPlatform(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{
		{ID: 1, Platform: PlatformKimi, Type: AccountTypeAPIKey},
		{ID: 2, Platform: PlatformDeepSeek, Type: AccountTypeAPIKey},
		{ID: 3, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2, 3},
		Extra: map[string]any{
			cnAPIProtocolExtraKey: APIProtocolResponses,
			cnAPIBaseURLsExtraKey: map[string]any{"responses": "https://legacy.example"},
			"feature":             true,
		},
		Credentials: map[string]any{
			"api_protocol":  APIProtocolAnthropic,
			"api_base_urls": map[string]any{"anthropic": "https://legacy.example"},
			"api_key":       "updated-key",
		},
	})

	require.NoError(t, err)
	require.Equal(t, 3, result.Success)
	require.Len(t, repo.bulkUpdateCalls, 3)

	kimi := repo.bulkUpdateCalls[0]
	require.Equal(t, []int64{1}, kimi.IDs)
	require.Equal(t, APIProtocolResponses, kimi.Updates.Extra[cnAPIProtocolExtraKey])
	require.Equal(t, map[string]any{"responses": "https://legacy.example"}, kimi.Updates.Extra[cnAPIBaseURLsExtraKey])
	require.NotContains(t, kimi.Updates.Credentials, "api_protocol")
	require.NotContains(t, kimi.Updates.Credentials, "api_base_urls")

	deepSeek := repo.bulkUpdateCalls[1]
	require.Equal(t, []int64{2}, deepSeek.IDs)
	require.Equal(t, APIProtocolResponses, deepSeek.Updates.Extra[cnAPIProtocolExtraKey])
	require.Equal(t, map[string]any{"responses": "https://legacy.example"}, deepSeek.Updates.Extra[cnAPIBaseURLsExtraKey])

	anthropic := repo.bulkUpdateCalls[2]
	require.Equal(t, []int64{3}, anthropic.IDs)
	require.NotContains(t, anthropic.Updates.Extra, cnAPIProtocolExtraKey)
	require.NotContains(t, anthropic.Updates.Extra, cnAPIBaseURLsExtraKey)
	require.NotContains(t, anthropic.Updates.Credentials, "api_protocol")
	require.NotContains(t, anthropic.Updates.Credentials, "api_base_urls")
	require.Equal(t, true, anthropic.Updates.Extra["feature"])
	require.Equal(t, "updated-key", anthropic.Updates.Credentials["api_key"])
}

func TestAdminServiceBulkUpdateAccounts_CleansLegacyCredentialsPerAccount(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{
		{ID: 1, Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_protocol": APIProtocolAnthropic}},
		{ID: 2, Platform: PlatformKimi, Type: AccountTypeAPIKey},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2},
		Extra:      map[string]any{cnAPIProtocolExtraKey: APIProtocolChatCompletions},
	})

	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.Len(t, repo.bulkUpdateCalls, 2)
	for _, call := range repo.bulkUpdateCalls {
		switch call.IDs[0] {
		case 1:
			require.Contains(t, call.Updates.Credentials, "api_protocol")
			require.Nil(t, call.Updates.Credentials["api_protocol"])
		case 2:
			require.NotContains(t, call.Updates.Credentials, "api_protocol")
		}
	}
}

func TestAdminServiceBulkUpdateAccounts_ClearsEmptyCNBaseURLs(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{{
		ID:       1,
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			cnAPIProtocolExtraKey: APIProtocolAdaptive,
			cnAPIBaseURLsExtraKey: map[string]any{"chat_completions": "https://old.example"},
		},
	}}}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1},
		Extra: map[string]any{
			cnAPIBaseURLsExtraKey: map[string]any{},
		},
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Nil(t, repo.bulkUpdateCalls[0].Updates.Extra[cnAPIBaseURLsExtraKey])
}

func TestAdminServiceBulkUpdateAccounts_LegacyCredentialProtocolOverridesStoredCNExtra(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{{
		ID:       1,
		Platform: PlatformDeepSeek,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{cnAPIProtocolExtraKey: APIProtocolChatCompletions},
	}}}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:  []int64{1},
		Credentials: map[string]any{"api_protocol": APIProtocolResponses},
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Len(t, repo.bulkUpdateCalls, 1)
	require.Equal(t, APIProtocolResponses, repo.bulkUpdateCalls[0].Updates.Extra[cnAPIProtocolExtraKey])
	require.NotContains(t, repo.bulkUpdateCalls[0].Updates.Credentials, "api_protocol")
}

func TestAdminServiceBulkUpdateAccounts_FirstTokenTimeoutStagesAreValidatedAndPreserved(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
	}}}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{7},
		Extra: map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderEnabledExtraKey: true,
			openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
				map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
				map[string]any{"stage": 2, "placeholder_ms": 3000, "guard_max_ms": 10000},
			},
		},
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.NotContains(t, repo.bulkUpdatePayload.ExtraRemoveKeys, openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey)
	require.Equal(t, 800, repo.bulkUpdatePayload.Extra[openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey])
	require.Equal(t, 5000, repo.bulkUpdatePayload.Extra[openAIAPIKeyFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey])
	require.Len(t, repo.bulkUpdatePayload.Extra[openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey], 2)
}

func TestAdminServiceBulkUpdateAccounts_LegacyScalarFirstTokenTimeoutEditDropsStagedPolicy(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
	}}}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{7},
		Extra: map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderEnabledExtraKey: true,
			openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey:      1200,
		},
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Contains(t, repo.bulkUpdatePayload.ExtraRemoveKeys, openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey)
}

func TestAdminServiceBulkUpdateAccounts_FirstTokenTimeoutRemovalOnlyAllowsUnsupportedTargets(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}
	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{7},
		ExtraRemoveKeys: []string{
			openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey,
			openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey,
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.ElementsMatch(t, []string{
		openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey,
		openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey,
	}, repo.bulkUpdatePayload.ExtraRemoveKeys)
}

func TestAdminServiceBulkUpdateAccounts_LegacyScalarFirstTokenTimeoutRejectsShadow(t *testing.T) {
	parentAccountID := int64(3)
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{{
		ID:              7,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeAPIKey,
		ParentAccountID: &parentAccountID,
	}}}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{7},
		Extra: map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderEnabledExtraKey: true,
			openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey:      800,
		},
	})

	require.Error(t, err)
	require.Nil(t, result)
	require.Empty(t, repo.bulkUpdateCalls)
}

func TestAdminServiceBulkUpdateAccounts_FirstTokenTimeoutStagesRejectUnsupportedTargets(t *testing.T) {
	parentAccountID := int64(3)
	tests := []struct {
		name    string
		account *Account
	}{
		{name: "OAuth", account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}},
		{name: "other platform", account: &Account{ID: 7, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}},
		{name: "image pool", account: &Account{
			ID:       7,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"pool_mode":       true,
				"image_pool_mode": true,
			},
		}},
		{name: "credential shadow", account: &Account{
			ID:              7,
			Platform:        PlatformOpenAI,
			Type:            AccountTypeAPIKey,
			ParentAccountID: &parentAccountID,
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{tt.account}}
			svc := &adminServiceImpl{accountRepo: repo}

			result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
				AccountIDs: []int64{7},
				Extra: map[string]any{
					openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
						map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
					},
				},
			})

			require.Error(t, err)
			require.Nil(t, result)
			require.Empty(t, repo.bulkUpdateCalls)
		})
	}
}

func TestAdminServiceBulkUpdateAccounts_SafeTokenEditKeepsStagedTimeoutPolicy(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{7},
		Extra: map[string]any{
			openAIAPIKeySafeTokenPlaceholderExtraKey: true,
		},
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.NotContains(t, repo.bulkUpdatePayload.ExtraRemoveKeys, openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey)
}

func TestAdminServiceBulkUpdateAccounts_LegacyProbeRemovalDisablesProbeAndRateSync(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{
		getByIDsAccounts: []*Account{{ID: 7, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{7},
		ExtraRemoveKeys: []string{
			UpstreamBillingProbeEnabledExtraKey,
			UpstreamBillingRateSyncEnabledExtraKey,
			UpstreamBillingProbeExtraKey,
			"codex_image_generation_bridge",
		},
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Equal(t, false, repo.bulkUpdatePayload.Extra[UpstreamBillingProbeEnabledExtraKey])
	require.Equal(t, false, repo.bulkUpdatePayload.Extra[UpstreamBillingRateSyncEnabledExtraKey])
	require.Equal(t, []string{"codex_image_generation_bridge"}, repo.bulkUpdatePayload.ExtraRemoveKeys)
}

func TestAdminServiceBulkUpdateAccounts_RejectsLegacyProbeRemovalWithExplicitEnable(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}
	enabled := true

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:      []int64{7},
		ProbeEnabled:    &enabled,
		ExtraRemoveKeys: []string{UpstreamBillingProbeEnabledExtraKey},
	})

	require.Nil(t, result)
	require.Error(t, err)
	require.Empty(t, repo.bulkUpdateCalls)
}

func TestAdminServiceBulkUpdateAccounts_SanitizesShadowUnsafeFields(t *testing.T) {
	parentID := int64(1)
	repo := &accountRepoStubForBulkUpdate{
		getByIDsAccounts: []*Account{
			{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID, QuotaDimension: QuotaDimensionSpark},
		},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	proxyID := int64(99)
	concurrency := 7
	input := &BulkUpdateAccountsInput{
		AccountIDs:      []int64{1, 2},
		ProxyID:         &proxyID,
		Concurrency:     &concurrency,
		Credentials:     map[string]any{"model_mapping": map[string]string{"spark": "spark", "": "drop", "blank": ""}, "custom_error_codes_enabled": true},
		Extra:           map[string]any{"openai_passthrough": true},
		ExtraRemoveKeys: []string{"codex_cli_only"},
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.Len(t, repo.bulkUpdateCalls, 2)

	normalCall := repo.bulkUpdateCalls[0]
	require.Equal(t, []int64{1}, normalCall.IDs)
	require.Equal(t, &proxyID, normalCall.Updates.ProxyID)
	require.Equal(t, map[string]any{"openai_passthrough": true}, normalCall.Updates.Extra)
	require.Equal(t, []string{"codex_cli_only"}, normalCall.Updates.ExtraRemoveKeys)
	require.Equal(t, true, normalCall.Updates.Credentials["custom_error_codes_enabled"])

	shadowCall := repo.bulkUpdateCalls[1]
	require.Equal(t, []int64{2}, shadowCall.IDs)
	require.Nil(t, shadowCall.Updates.ProxyID)
	require.Nil(t, shadowCall.Updates.Extra)
	require.Empty(t, shadowCall.Updates.ExtraRemoveKeys)
	require.Equal(t, &concurrency, shadowCall.Updates.Concurrency)
	require.Equal(t, map[string]any{"model_mapping": map[string]any{"spark": "spark"}}, shadowCall.Updates.Credentials)
}

func TestAdminServiceBulkUpdateAccounts_PropagatesParentProxyToShadows(t *testing.T) {
	parentID := int64(1)
	repo := &accountRepoStubForBulkUpdate{
		getByIDsAccounts: []*Account{
			{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		},
		shadowsByParent: map[int64][]*Account{
			parentID: {
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID, QuotaDimension: QuotaDimensionSpark},
			},
		},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	proxyID := int64(99)
	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{parentID},
		ProxyID:    &proxyID,
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Len(t, repo.updateCalls, 1)
	require.Equal(t, int64(2), repo.updateCalls[0].ID)
	require.NotNil(t, repo.updateCalls[0].ProxyID)
	require.Equal(t, proxyID, *repo.updateCalls[0].ProxyID)
}

func (s *accountRepoStubForBulkUpdate) BindGroups(_ context.Context, accountID int64, _ []int64) error {
	s.bindGroupsCalls = append(s.bindGroupsCalls, accountID)
	if err, ok := s.bindGroupErrByID[accountID]; ok {
		return err
	}
	return nil
}

func (s *accountRepoStubForBulkUpdate) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	s.getByIDsCalled = true
	s.getByIDsIDs = append([]int64{}, ids...)
	if s.getByIDsErr != nil {
		return nil, s.getByIDsErr
	}
	return s.getByIDsAccounts, nil
}

func (s *accountRepoStubForBulkUpdate) GetByID(_ context.Context, id int64) (*Account, error) {
	s.getByIDCalled = append(s.getByIDCalled, id)
	if err, ok := s.getByIDErrByID[id]; ok {
		return nil, err
	}
	if account, ok := s.getByIDAccounts[id]; ok {
		return account, nil
	}
	return nil, errors.New("account not found")
}

func (s *accountRepoStubForBulkUpdate) ListByGroup(_ context.Context, groupID int64) ([]Account, error) {
	if err, ok := s.listByGroupErr[groupID]; ok {
		return nil, err
	}
	if rows, ok := s.listByGroupData[groupID]; ok {
		return rows, nil
	}
	return nil, nil
}

func (s *accountRepoStubForBulkUpdate) ListWithFilters(_ context.Context, params pagination.PaginationParams, platform, accountType, status, search string, groupID int64, privacyMode, poolMode string) ([]Account, *pagination.PaginationResult, error) {
	s.listCalled = true
	s.lastListParams = params
	s.lastListFilters.platform = platform
	s.lastListFilters.accountType = accountType
	s.lastListFilters.status = status
	s.lastListFilters.search = search
	s.lastListFilters.groupID = groupID
	s.lastListFilters.privacyMode = privacyMode
	if s.listErr != nil {
		return nil, nil, s.listErr
	}
	if s.listResult != nil {
		return s.listData, s.listResult, nil
	}
	return s.listData, &pagination.PaginationResult{Total: int64(len(s.listData))}, nil
}

// TestAdminService_BulkUpdateAccounts_AllSuccessIDs 验证批量更新成功时返回 success_ids/failed_ids。
func TestAdminService_BulkUpdateAccounts_AllSuccessIDs(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	schedulable := true
	input := &BulkUpdateAccountsInput{
		AccountIDs:  []int64{1, 2, 3},
		Schedulable: &schedulable,
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, 3, result.Success)
	require.Equal(t, 0, result.Failed)
	require.ElementsMatch(t, []int64{1, 2, 3}, result.SuccessIDs)
	require.Empty(t, result.FailedIDs)
	require.Len(t, result.Results, 3)
}

// TestAdminService_BulkUpdateAccounts_PartialFailureIDs 验证部分失败时 success_ids/failed_ids 正确。
func TestAdminService_BulkUpdateAccounts_PartialFailureIDs(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{
		bindGroupErrByID: map[int64]error{
			2: errors.New("bind failed"),
		},
	}
	svc := &adminServiceImpl{
		accountRepo: repo,
		groupRepo:   &groupRepoStubForAdmin{getByID: &Group{ID: 10, Name: "g10"}},
	}

	groupIDs := []int64{10}
	schedulable := false
	input := &BulkUpdateAccountsInput{
		AccountIDs:            []int64{1, 2, 3},
		GroupIDs:              &groupIDs,
		Schedulable:           &schedulable,
		SkipMixedChannelCheck: true,
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.Equal(t, 1, result.Failed)
	require.ElementsMatch(t, []int64{1, 3}, result.SuccessIDs)
	require.ElementsMatch(t, []int64{2}, result.FailedIDs)
	require.Len(t, result.Results, 3)
}

func TestAdminService_BulkUpdateAccounts_RuntimeClearFailureDoesNotSkipGroupBindings(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	blocker := &failingCrossReplicaRuntimeBlocker{}
	svc := &adminServiceImpl{
		accountRepo:    repo,
		groupRepo:      &groupRepoStubForAdmin{getByID: &Group{ID: 10, Name: "g10"}},
		runtimeBlocker: blocker,
	}
	groupIDs := []int64{10}
	schedulable := false

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:            []int64{1, 2},
		GroupIDs:              &groupIDs,
		Schedulable:           &schedulable,
		SkipMixedChannelCheck: true,
	})

	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, repo.bindGroupsCalls)
	require.Equal(t, []int64{1, 2}, blocker.clears)
	require.Zero(t, result.Success)
	require.Equal(t, 2, result.Failed)
	require.ElementsMatch(t, []int64{1, 2}, result.FailedIDs)
}

func TestAdminService_BulkUpdateAccounts_NilGroupRepoReturnsError(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	groupIDs := []int64{10}
	input := &BulkUpdateAccountsInput{
		AccountIDs: []int64{1},
		GroupIDs:   &groupIDs,
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "group repository not configured")
}

// TestAdminService_BulkUpdateAccounts_MixedChannelPreCheckBlocksOnExistingConflict verifies
// that the global pre-check detects a conflict with existing group members and returns an
// error before any DB write is performed.
func TestAdminService_BulkUpdateAccounts_MixedChannelPreCheckBlocksOnExistingConflict(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{
		getByIDsAccounts: []*Account{
			{ID: 1, Platform: PlatformAntigravity},
		},
		// Group 10 already contains an Anthropic account.
		listByGroupData: map[int64][]Account{
			10: {{ID: 99, Platform: PlatformAnthropic}},
		},
	}
	svc := &adminServiceImpl{
		accountRepo: repo,
		groupRepo:   &groupRepoStubForAdmin{getByID: &Group{ID: 10, Name: "target-group"}},
	}

	groupIDs := []int64{10}
	input := &BulkUpdateAccountsInput{
		AccountIDs: []int64{1},
		GroupIDs:   &groupIDs,
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mixed channel")
	// No BindGroups should have been called since the check runs before any write.
	require.Empty(t, repo.bindGroupsCalls)
}

func TestAdminServiceBulkUpdateAccounts_ResolvesIDsFromFilters(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{
		listData: []Account{
			{ID: 7},
			{ID: 11},
		},
		listResult: &pagination.PaginationResult{Total: 2},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	schedulable := true
	input := &BulkUpdateAccountsInput{
		Schedulable: &schedulable,
	}

	filtersField := reflect.ValueOf(input).Elem().FieldByName("Filters")
	require.True(t, filtersField.IsValid(), "BulkUpdateAccountsInput should expose Filters for filter-target bulk update")
	require.Equal(t, reflect.Ptr, filtersField.Kind(), "BulkUpdateAccountsInput.Filters should be a pointer field")

	filtersValue := reflect.New(filtersField.Type().Elem())
	filtersValue.Elem().FieldByName("Platform").SetString(PlatformOpenAI)
	filtersValue.Elem().FieldByName("Type").SetString(AccountTypeOAuth)
	filtersValue.Elem().FieldByName("Status").SetString(StatusActive)
	filtersValue.Elem().FieldByName("Group").SetString("12")
	filtersValue.Elem().FieldByName("PrivacyMode").SetString(PrivacyModeCFBlocked)
	filtersValue.Elem().FieldByName("Search").SetString("bulk-target")
	filtersField.Set(filtersValue)

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.NoError(t, err)
	require.True(t, repo.listCalled, "expected filter-target bulk update to resolve matching IDs via account list filters")
	require.Equal(t, PlatformOpenAI, repo.lastListFilters.platform)
	require.Equal(t, AccountTypeOAuth, repo.lastListFilters.accountType)
	require.Equal(t, StatusActive, repo.lastListFilters.status)
	require.Equal(t, "bulk-target", repo.lastListFilters.search)
	require.Equal(t, int64(12), repo.lastListFilters.groupID)
	require.Equal(t, PrivacyModeCFBlocked, repo.lastListFilters.privacyMode)
	require.Equal(t, []int64{7, 11}, repo.bulkUpdateIDs)
	require.Equal(t, 2, result.Success)
	require.Equal(t, 0, result.Failed)
	require.Equal(t, []int64{7, 11}, result.SuccessIDs)
}
