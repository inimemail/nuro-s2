package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type accountDuplicateRepoStub struct {
	AccountRepository
	source *Account
	copies []*Account
	groups []AccountGroup
	err    error
}

func (r *accountDuplicateRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.source == nil || id != r.source.ID {
		return nil, ErrAccountNotFound
	}
	return r.source, nil
}
func (r *accountDuplicateRepoStub) FindByExtraField(_ context.Context, key string, value any) ([]Account, error) {
	var out []Account
	for _, account := range r.copies {
		if account.Extra[key] == value {
			out = append(out, *account)
		}
	}
	return out, nil
}
func (r *accountDuplicateRepoStub) CreateWithAccountGroups(_ context.Context, account *Account, groups []AccountGroup) error {
	if r.err != nil {
		return r.err
	}
	account.ID = int64(100 + len(r.copies))
	r.copies = append(r.copies, account)
	r.groups = groups
	return nil
}

func TestDuplicateAccountPreservesLocalConfigWithoutSourceState(t *testing.T) {
	proxy, fallback, parent := int64(7), int64(9), int64(3)
	rate, guardMax, guardMin := 0.0, 1.2, 0.3
	now := time.Now()
	source := &Account{
		ID: 42, Name: "source", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "private-key", "model_mapping": map[string]any{"a": "b"}},
		Extra: map[string]any{
			openAIAPIKeyPreambleFlushExtraKey: true, openAIAPIKeySSECommentPreflushExtraKey: true,
			openAIAPIKeySafeTokenPlaceholderExtraKey:               true,
			openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey: []any{map[string]any{"timeout_ms": 600}},
			"quota_limit": 100.0, "quota_used": 90.0, "quota_daily_used": 20.0,
			"quota_daily_reset_mode": "fixed", "quota_daily_reset_hour": 1,
			"quota_daily_reset_at": "2000-01-01T00:00:00Z", "quota_reset_timezone": "UTC",
			UpstreamBillingProbeEnabledExtraKey: true, UpstreamBillingRateSyncEnabledExtraKey: true,
			UpstreamBillingProbeExtraKey:     map[string]any{"status": "failed"},
			ManualUpstreamMultiplierExtraKey: 0.5, AdaptiveUpstreamMultiplierFactorExtraKey: 0.1,
			"openai_downstream_cache_usage_mode": "D", "crs_account_id": "external-id",
			"openai_compact_supported": false, "model_rate_limits": map[string]any{"a": "cooldown"},
			codexFingerprintSeedExtraKey: "old-seed", "opencode_go_5h_used_percent": 80.0,
			OllamaCloudUsageSnapshotExtraKey: map[string]any{"error": "old"},
		},
		ProxyID: &proxy, ProxyFallbackOriginID: &fallback, Concurrency: 9, Priority: 3, RateMultiplier: &rate,
		Status: StatusError, Schedulable: true, ErrorMessage: "old error", LastUsedAt: &now,
		ExpiresAt: &now, AutoPauseOnExpired: true, RateLimitResetAt: &now,
		UpstreamBillingGuardEnabled: true, UpstreamBillingGuardMaxMultiplier: guardMax, UpstreamBillingGuardBlocked: true,
		AccountGroups: []AccountGroup{{GroupID: parent, Priority: 11,
			UpstreamBillingGuardOverrideMaxMultiplier: &guardMax, UpstreamBillingGuardOverrideMinMultiplier: &guardMin}},
	}
	repo := &accountDuplicateRepoStub{source: source}
	svc := &adminServiceImpl{accountRepo: repo}
	copy, err := svc.DuplicateAccount(context.Background(), source.ID, "admin:1", "operation")
	require.NoError(t, err)
	require.NotEqual(t, source.ID, copy.ID)
	require.Equal(t, "source (Copy)", copy.Name)
	require.Equal(t, StatusActive, copy.Status)
	require.False(t, copy.Schedulable)
	require.Empty(t, copy.ErrorMessage)
	require.Nil(t, copy.RateLimitResetAt)
	require.Nil(t, copy.LastUsedAt)
	require.True(t, copy.UpstreamBillingGuardEnabled)
	require.False(t, copy.UpstreamBillingGuardBlocked)
	require.Equal(t, fallback, *copy.ProxyID)
	require.Nil(t, copy.ProxyFallbackOriginID)
	require.Equal(t, rate, *copy.RateMultiplier)
	require.Equal(t, source.Concurrency, copy.Concurrency)
	require.Equal(t, source.Priority, copy.Priority)
	for _, key := range []string{openAIAPIKeyPreambleFlushExtraKey, openAIAPIKeySSECommentPreflushExtraKey, openAIAPIKeySafeTokenPlaceholderExtraKey, UpstreamBillingProbeEnabledExtraKey, UpstreamBillingRateSyncEnabledExtraKey, ManualUpstreamMultiplierExtraKey, AdaptiveUpstreamMultiplierFactorExtraKey, "quota_limit", "openai_downstream_cache_usage_mode"} {
		require.Equal(t, source.Extra[key], copy.Extra[key], key)
	}
	for _, key := range []string{"quota_used", "quota_daily_used", "crs_account_id", "model_rate_limits", "openai_compact_supported", codexFingerprintSeedExtraKey, "opencode_go_5h_used_percent", UpstreamBillingProbeExtraKey, OllamaCloudUsageSnapshotExtraKey} {
		require.NotContains(t, copy.Extra, key)
		require.Contains(t, source.Extra, key)
	}
	reset, err := time.Parse(time.RFC3339, copy.Extra["quota_daily_reset_at"].(string))
	require.NoError(t, err)
	require.True(t, reset.After(now))
	require.Equal(t, 11, repo.groups[0].Priority)
	require.Equal(t, guardMax, *repo.groups[0].UpstreamBillingGuardOverrideMaxMultiplier)
	*repo.groups[0].UpstreamBillingGuardOverrideMinMultiplier = 9
	require.Equal(t, 0.3, guardMin)
	copy.Credentials["model_mapping"].(map[string]any)["a"] = "changed"
	copy.Extra[openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey].([]any)[0].(map[string]any)["timeout_ms"] = 999
	require.Equal(t, "b", source.Credentials["model_mapping"].(map[string]any)["a"])
	require.Equal(t, 600, source.Extra[openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey].([]any)[0].(map[string]any)["timeout_ms"])
}

func TestDuplicateAccountEligibilityAndIdempotency(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{AccountTypeOAuth, AccountTypeSetupToken, "unknown"} {
		repo := &accountDuplicateRepoStub{source: &Account{ID: 1, Type: kind}}
		_, err := (&adminServiceImpl{accountRepo: repo}).DuplicateAccount(ctx, 1, "admin:1", "")
		require.Equal(t, "ACCOUNT_DUPLICATE_CREDENTIAL_TYPE_UNSUPPORTED", infraerrors.Reason(err))
		require.Empty(t, repo.copies)
	}
	parent := int64(2)
	repo := &accountDuplicateRepoStub{source: &Account{ID: 1, Type: AccountTypeAPIKey, ParentAccountID: &parent}}
	svc := &adminServiceImpl{accountRepo: repo}
	_, err := svc.DuplicateAccount(ctx, 1, "admin:1", "")
	require.Equal(t, "ACCOUNT_DUPLICATE_SHADOW_UNSUPPORTED", infraerrors.Reason(err))
	repo.source.ParentAccountID = nil
	repo.source.GroupIDs = []int64{3, 4}
	first, err := svc.DuplicateAccount(ctx, 1, "admin:1", "key")
	require.NoError(t, err)
	require.Equal(t, []AccountGroup{{GroupID: 3, Priority: 1}, {GroupID: 4, Priority: 2}}, repo.groups)
	again, err := svc.DuplicateAccount(ctx, 1, "admin:1", "key")
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
	require.Len(t, repo.copies, 1)
	other, err := svc.RecoverDuplicateAccount(ctx, 1, "admin:2", "key")
	require.NoError(t, err)
	require.Nil(t, other)
	repo.err = errors.New("transaction rolled back")
	_, err = svc.DuplicateAccount(ctx, 1, "admin:1", "new-key")
	require.ErrorIs(t, err, repo.err)
	require.Len(t, repo.copies, 1)
	_, err = svc.DuplicateAccount(ctx, 999, "admin:1", "")
	require.ErrorIs(t, err, ErrAccountNotFound)
	name := duplicateAccountName(strings.Repeat("界", 100))
	require.LessOrEqual(t, len(name), 100)
	require.True(t, utf8.ValidString(name))
	require.True(t, strings.HasSuffix(name, " (Copy)"))
}
