package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type upstreamBillingProbeAccountRepoStub struct {
	AccountRepository
	account      *Account
	updatedExtra map[string]any
	lastSnapshot *UpstreamBillingProbeSnapshot
	lastObserved *float64
	guardUpdates []UpstreamBillingGuardSettings
}

func (r *upstreamBillingProbeAccountRepoStub) GetByID(context.Context, int64) (*Account, error) {
	if r.account == nil {
		return nil, ErrAccountNotFound
	}
	copy := *r.account
	return &copy, nil
}

func (r *upstreamBillingProbeAccountRepoStub) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updatedExtra == nil {
		r.updatedExtra = make(map[string]any)
	}
	for key, value := range updates {
		r.updatedExtra[key] = value
	}
	return nil
}

func (r *upstreamBillingProbeAccountRepoStub) UpdateUpstreamBillingProbeSnapshot(_ context.Context, _ *Account, snapshot *UpstreamBillingProbeSnapshot, observed *float64) (bool, error) {
	r.lastSnapshot = snapshot
	r.lastObserved = observed
	return false, nil
}

func (r *upstreamBillingProbeAccountRepoStub) UpdateUpstreamBillingProbeEnabled(_ context.Context, _ int64, enabled bool) error {
	if !enabled && r.account.UpstreamBillingGuardEnabled {
		return ErrUpstreamBillingProbeRequiredByGuard
	}
	if r.account.Extra == nil {
		r.account.Extra = make(map[string]any)
	}
	r.account.Extra[UpstreamBillingProbeEnabledExtraKey] = enabled
	r.updatedExtra = map[string]any{UpstreamBillingProbeEnabledExtraKey: enabled}
	return nil
}

func (r *upstreamBillingProbeAccountRepoStub) UpdateUpstreamBillingGuard(_ context.Context, _ int64, enabled bool, maxMultiplier float64) (*Account, bool, error) {
	r.guardUpdates = append(r.guardUpdates, UpstreamBillingGuardSettings{Enabled: enabled, MaxMultiplier: maxMultiplier})
	updated := *r.account
	updated.UpstreamBillingGuardEnabled = enabled
	updated.UpstreamBillingGuardMaxMultiplier = maxMultiplier
	updated.UpstreamBillingGuardBlocked = enabled && updated.UpstreamBillingGuardObservedMultiplier != nil && *updated.UpstreamBillingGuardObservedMultiplier > maxMultiplier
	r.account = &updated
	return &updated, true, nil
}

type upstreamBillingSettingRepo struct{ values map[string]string }

func (r *upstreamBillingSettingRepo) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}
func (r *upstreamBillingSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}
func (r *upstreamBillingSettingRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}
func (r *upstreamBillingSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (r *upstreamBillingSettingRepo) SetMultiple(context.Context, map[string]string) error {
	return nil
}
func (r *upstreamBillingSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return r.values, nil
}
func (r *upstreamBillingSettingRepo) Delete(_ context.Context, key string) error {
	delete(r.values, key)
	return nil
}

func TestUpstreamBillingProbeSettingsDefaultDisabledAndPersisted(t *testing.T) {
	repo := &upstreamBillingSettingRepo{values: make(map[string]string)}
	svc := &SettingService{settingRepo: repo}
	settings, err := svc.GetUpstreamBillingProbeSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled)
	require.Equal(t, 5, settings.IntervalSeconds)

	require.NoError(t, svc.SetUpstreamBillingProbeSettings(context.Background(), &UpstreamBillingProbeSettings{Enabled: true, IntervalSeconds: 15}))
	var stored UpstreamBillingProbeSettings
	require.NoError(t, json.Unmarshal([]byte(repo.values[SettingKeyUpstreamBillingProbeSettings]), &stored))
	require.True(t, stored.Enabled)
	require.Equal(t, 15, stored.IntervalSeconds)

	repo.values[SettingKeyUpstreamBillingProbeSettings] = `{"enabled":true,"interval_minutes":30}`
	legacy, err := svc.GetUpstreamBillingProbeSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1800, legacy.IntervalSeconds)
}

func TestParseUpstreamBillingProbeResponseSanitizesAndValidates(t *testing.T) {
	body := []byte(`{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":1.2,"resolved_rate_multiplier":1.2,"peak_rate_enabled":false,"effective_rate_multiplier":1.2,"observed_at":"2026-07-16T04:00:00Z","ignored_secret":"do-not-store"}`)
	data, err := parseUpstreamBillingProbeResponse(body)
	require.NoError(t, err)
	require.Equal(t, 1.2, data["effective_rate_multiplier"])
	require.NotContains(t, data, "ignored_secret")

	bad := []byte(`{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":1.2,"resolved_rate_multiplier":1.2,"peak_rate_enabled":false,"effective_rate_multiplier":9,"observed_at":"2026-07-16T04:00:00Z"}`)
	_, err = parseUpstreamBillingProbeResponse(bad)
	require.Error(t, err)
}

func TestSafeProbeErrorDoesNotExposeInternalDetails(t *testing.T) {
	require.Equal(t, "probe_failed", safeProbeError(context.DeadlineExceeded))
}

func TestUpstreamBillingProbeSettingsRejectOutOfRangeSeconds(t *testing.T) {
	repo := &upstreamBillingSettingRepo{values: make(map[string]string)}
	svc := &SettingService{settingRepo: repo}
	for _, seconds := range []int{0, upstreamBillingProbeMaxIntervalSeconds + 1} {
		err := svc.SetUpstreamBillingProbeSettings(context.Background(), &UpstreamBillingProbeSettings{IntervalSeconds: seconds})
		require.Error(t, err)
	}
	repo.values[SettingKeyUpstreamBillingProbeSettings] = `{"enabled":true,"interval_seconds":999999}`
	settings, err := svc.GetUpstreamBillingProbeSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, upstreamBillingProbeMaxIntervalSeconds, settings.IntervalSeconds)
}

func TestUpstreamBillingGuardRequiresAutomaticProbeAndReevaluatesThreshold(t *testing.T) {
	observed := 2.0
	repo := &upstreamBillingProbeAccountRepoStub{account: &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra:                                  map[string]any{UpstreamBillingProbeEnabledExtraKey: false},
		UpstreamBillingGuardObservedMultiplier: &observed,
	}}
	svc := NewUpstreamBillingProbeService(repo, nil, nil)
	_, err := svc.UpdateGuard(context.Background(), 7, UpstreamBillingGuardSettings{Enabled: true, MaxMultiplier: 1.5})
	require.ErrorIs(t, err, ErrUpstreamBillingGuardRequiresAutoProbe)

	repo.account.Extra[UpstreamBillingProbeEnabledExtraKey] = true
	result, err := svc.UpdateGuard(context.Background(), 7, UpstreamBillingGuardSettings{Enabled: true, MaxMultiplier: 1.5})
	require.NoError(t, err)
	require.True(t, result.Account.UpstreamBillingGuardBlocked)
	require.Len(t, repo.guardUpdates, 1)

	result, err = svc.UpdateGuard(context.Background(), 7, UpstreamBillingGuardSettings{Enabled: true, MaxMultiplier: 2.0})
	require.NoError(t, err)
	require.False(t, result.Account.UpstreamBillingGuardBlocked)

	repo.account.UpstreamBillingGuardEnabled = false
	err = svc.SetAccountEnabled(context.Background(), 7, true)
	require.NoError(t, err)
	repo.account.UpstreamBillingGuardEnabled = true
	err = svc.SetAccountEnabled(context.Background(), 7, false)
	require.ErrorIs(t, err, ErrUpstreamBillingProbeRequiredByGuard)
}

func TestUpstreamBillingProbeFailurePreservesGuardState(t *testing.T) {
	observed := 4.0
	repo := &upstreamBillingProbeAccountRepoStub{account: &Account{
		ID: 8, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		UpstreamBillingGuardEnabled: true, UpstreamBillingGuardMaxMultiplier: 1.0,
		UpstreamBillingGuardBlocked: true, UpstreamBillingGuardObservedMultiplier: &observed,
	}}
	svc := NewUpstreamBillingProbeService(repo, nil, nil)
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	snapshot, err := svc.persistFailure(context.Background(), repo.account, 1, now, 502, "http_error")
	require.NoError(t, err)
	require.Equal(t, "failed", snapshot.Status)
	require.True(t, repo.account.UpstreamBillingGuardBlocked)
	require.Nil(t, repo.lastObserved)
	require.Equal(t, 10*time.Second, snapshot.NextProbeAt.Sub(now))
}

func TestNormalizeAccountUpdateExtraPreservesProbeRuntimeState(t *testing.T) {
	storedSnapshot := map[string]any{
		"status":          "ok",
		"last_attempt_at": "2026-07-17T00:00:00Z",
	}
	account := &Account{
		Platform: PlatformOpenAI,
		Extra: map[string]any{
			UpstreamBillingProbeEnabledExtraKey: true,
			UpstreamBillingProbeExtraKey:        storedSnapshot,
		},
	}

	normalized, err := normalizeOpenAILongContextBillingUpdateExtra(account, &UpdateAccountInput{
		Extra: map[string]any{
			"unrelated_setting":          true,
			UpstreamBillingProbeExtraKey: map[string]any{"status": "stale"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, true, normalized[UpstreamBillingProbeEnabledExtraKey])
	require.Equal(t, storedSnapshot, normalized[UpstreamBillingProbeExtraKey])

	normalized, err = normalizeOpenAILongContextBillingUpdateExtra(account, &UpdateAccountInput{
		Extra: map[string]any{UpstreamBillingProbeEnabledExtraKey: false},
	})
	require.NoError(t, err)
	require.Equal(t, false, normalized[UpstreamBillingProbeEnabledExtraKey])
	require.Equal(t, storedSnapshot, normalized[UpstreamBillingProbeExtraKey])

	_, err = normalizeOpenAILongContextBillingUpdateExtra(account, &UpdateAccountInput{
		Extra: map[string]any{UpstreamBillingProbeEnabledExtraKey: "false"},
	})
	require.Error(t, err)
}
