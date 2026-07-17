package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

const upstreamBillingProbeSuccessBody = `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":1.2,"resolved_rate_multiplier":1.2,"peak_rate_enabled":false,"effective_rate_multiplier":1.2,"observed_at":"2026-07-16T04:00:00Z"}`

type upstreamBillingProbeAccountRepoStub struct {
	AccountRepository
	account      *Account
	updatedExtra map[string]any
	lastSnapshot *UpstreamBillingProbeSnapshot
	lastObserved *float64
	guardUpdates []UpstreamBillingGuardSettings
}

type upstreamBillingProbeConcurrentRepo struct {
	AccountRepository
	accounts map[int64]*Account
}

func cloneUpstreamBillingProbeTestMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func (r *upstreamBillingProbeConcurrentRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	account := r.accounts[id]
	if account == nil {
		return nil, ErrAccountNotFound
	}
	copy := *account
	copy.Extra = cloneUpstreamBillingProbeTestMap(account.Extra)
	copy.Credentials = cloneUpstreamBillingProbeTestMap(account.Credentials)
	return &copy, nil
}

func (r *upstreamBillingProbeConcurrentRepo) FindByExtraField(ctx context.Context, _ string, _ any) ([]Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	accounts := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		copy := *account
		copy.Extra = cloneUpstreamBillingProbeTestMap(account.Extra)
		copy.Credentials = cloneUpstreamBillingProbeTestMap(account.Credentials)
		accounts = append(accounts, copy)
	}
	return accounts, nil
}

func (r *upstreamBillingProbeConcurrentRepo) UpdateUpstreamBillingProbeSnapshot(ctx context.Context, _ *Account, _ *UpstreamBillingProbeSnapshot, _ *float64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, nil
}

type upstreamBillingProbeBlockingUpstream struct {
	started chan struct{}
	release chan struct{}
	err     error
	calls   atomic.Int32
	active  atomic.Int32
	max     atomic.Int32
}

func newUpstreamBillingProbeBlockingUpstream() *upstreamBillingProbeBlockingUpstream {
	return &upstreamBillingProbeBlockingUpstream{
		started: make(chan struct{}, UpstreamBillingProbeMaxBatchSize),
		release: make(chan struct{}),
	}
}

func (u *upstreamBillingProbeBlockingUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	active := u.active.Add(1)
	for {
		maximum := u.max.Load()
		if active <= maximum || u.max.CompareAndSwap(maximum, active) {
			break
		}
	}
	u.started <- struct{}{}
	select {
	case <-u.release:
	case <-req.Context().Done():
		u.active.Add(-1)
		return nil, req.Context().Err()
	}
	u.active.Add(-1)
	if u.err != nil {
		return nil, u.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(upstreamBillingProbeSuccessBody)),
	}, nil
}

func (u *upstreamBillingProbeBlockingUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func upstreamBillingProbeTestAccounts(count int) map[int64]*Account {
	accounts := make(map[int64]*Account, count)
	for i := 1; i <= count; i++ {
		id := int64(i)
		accounts[id] = &Account{
			ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
			Credentials: map[string]any{"api_key": "test-key"},
			Extra:       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		}
	}
	return accounts
}

func newConcurrentUpstreamBillingProbeService(repo AccountRepository, upstream HTTPUpstream) *UpstreamBillingProbeService {
	return NewUpstreamBillingProbeService(repo, &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{},
	}, nil)
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

func (r *upstreamBillingProbeAccountRepoStub) UpdateUpstreamBillingGuard(_ context.Context, _ int64, enabled bool) (*Account, bool, error) {
	r.guardUpdates = append(r.guardUpdates, UpstreamBillingGuardSettings{Enabled: enabled})
	updated := *r.account
	updated.UpstreamBillingGuardEnabled = enabled
	updated.UpstreamBillingGuardBlocked = false
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

func TestUpstreamBillingProbeConstructorUsesConfiguredConcurrency(t *testing.T) {
	svc := NewUpstreamBillingProbeService(nil, nil, nil)
	require.Equal(t, 16, upstreamBillingProbeConcurrency)
	require.Equal(t, upstreamBillingProbeConcurrency, cap(svc.probeSlots))
}

func TestUpstreamBillingProbeRunDueRespectsConcurrencyLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := &upstreamBillingProbeConcurrentRepo{accounts: upstreamBillingProbeTestAccounts(UpstreamBillingProbeMaxBatchSize)}
	upstream := newUpstreamBillingProbeBlockingUpstream()
	settingsRepo := &upstreamBillingSettingRepo{values: map[string]string{
		SettingKeyUpstreamBillingProbeSettings: `{"enabled":true,"interval_seconds":5}`,
	}}
	svc := newConcurrentUpstreamBillingProbeService(repo, upstream)
	svc.settingService = &SettingService{settingRepo: settingsRepo}
	svc.db = db

	done := make(chan error, 1)
	go func() { done <- svc.RunDue(context.Background()) }()
	for i := 0; i < upstreamBillingProbeConcurrency; i++ {
		select {
		case <-upstream.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for scheduled probe %d", i+1)
		}
	}
	select {
	case <-upstream.started:
		t.Fatal("scheduled probes exceeded the configured concurrency")
	case <-time.After(50 * time.Millisecond):
	}
	require.Equal(t, int32(upstreamBillingProbeConcurrency), upstream.active.Load())
	require.Equal(t, int32(upstreamBillingProbeConcurrency), upstream.max.Load())

	close(upstream.release)
	require.NoError(t, <-done)
	require.LessOrEqual(t, upstream.max.Load(), int32(upstreamBillingProbeConcurrency))
	require.Equal(t, 0, len(svc.probeSlots))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpstreamBillingProbeManualAndScheduledDeduplicateSameAccount(t *testing.T) {
	repo := &upstreamBillingProbeConcurrentRepo{accounts: upstreamBillingProbeTestAccounts(1)}
	upstream := newUpstreamBillingProbeBlockingUpstream()
	svc := newConcurrentUpstreamBillingProbeService(repo, upstream)

	manualDone := make(chan error, 1)
	go func() {
		_, err := svc.ProbeAccount(context.Background(), 1)
		manualDone <- err
	}()
	select {
	case <-upstream.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for manual probe")
	}

	scheduledDone := make(chan error, 1)
	go func() {
		_, err := svc.probeScheduledAccount(context.Background(), 1, 5)
		scheduledDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(upstream.release)

	require.NoError(t, <-manualDone)
	require.NoError(t, <-scheduledDone)
	require.Equal(t, int32(1), upstream.calls.Load())
	require.Equal(t, 0, len(svc.probeSlots))
}

func TestUpstreamBillingProbeCancellationReleasesSlot(t *testing.T) {
	repo := &upstreamBillingProbeConcurrentRepo{accounts: upstreamBillingProbeTestAccounts(1)}
	upstream := newUpstreamBillingProbeBlockingUpstream()
	svc := newConcurrentUpstreamBillingProbeService(repo, upstream)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := svc.ProbeAccount(ctx, 1)
		done <- err
	}()
	select {
	case <-upstream.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancellable probe")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, int32(0), upstream.active.Load())
	require.Equal(t, 0, len(svc.probeSlots))
}

func TestUpstreamBillingProbeFailureReleasesSlot(t *testing.T) {
	repo := &upstreamBillingProbeConcurrentRepo{accounts: upstreamBillingProbeTestAccounts(1)}
	upstream := newUpstreamBillingProbeBlockingUpstream()
	upstream.err = errors.New("upstream unavailable")
	svc := newConcurrentUpstreamBillingProbeService(repo, upstream)

	done := make(chan error, 1)
	go func() {
		_, err := svc.ProbeAccount(context.Background(), 1)
		done <- err
	}()
	select {
	case <-upstream.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failing probe")
	}
	close(upstream.release)
	require.NoError(t, <-done)
	require.Equal(t, int32(0), upstream.active.Load())
	require.Equal(t, 0, len(svc.probeSlots))
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

func TestUpstreamBillingGuardEnablesAutomaticProbeAndMasterSwitch(t *testing.T) {
	observed := 2.0
	limit := 3.0
	repo := &upstreamBillingProbeAccountRepoStub{account: &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra:                                  map[string]any{UpstreamBillingProbeEnabledExtraKey: false},
		UpstreamBillingGuardObservedMultiplier: &observed,
		AccountGroups: []AccountGroup{{
			GroupID: 11, UpstreamBillingGuardMaxMultiplier: &limit,
		}},
	}}
	svc := NewUpstreamBillingProbeService(repo, nil, nil)
	result, err := svc.UpdateGuard(context.Background(), 7, UpstreamBillingGuardSettings{Enabled: true})
	require.NoError(t, err)
	require.True(t, result.Account.UpstreamBillingGuardEnabled)
	require.False(t, result.Account.UpstreamBillingGuardBlocked)
	require.Equal(t, true, repo.account.Extra[UpstreamBillingProbeEnabledExtraKey])
	require.Len(t, repo.guardUpdates, 1)

	repo.account.UpstreamBillingGuardEnabled = false
	err = svc.SetAccountEnabled(context.Background(), 7, true)
	require.NoError(t, err)
	repo.account.UpstreamBillingGuardEnabled = true
	err = svc.SetAccountEnabled(context.Background(), 7, false)
	require.ErrorIs(t, err, ErrUpstreamBillingProbeRequiredByGuard)
}

func TestUpstreamBillingGuardRejectsEnableWithoutConfiguredGroup(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepoStub{account: &Account{
		ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
	}}
	svc := NewUpstreamBillingProbeService(repo, nil, nil)

	_, err := svc.UpdateGuard(context.Background(), 9, UpstreamBillingGuardSettings{Enabled: true})

	require.ErrorIs(t, err, ErrUpstreamBillingGuardRequiresGroupLimit)
	require.Empty(t, repo.guardUpdates)
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
