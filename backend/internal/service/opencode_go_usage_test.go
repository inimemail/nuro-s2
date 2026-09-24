//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type v028OpenCodeHTTP struct {
	HTTPUpstream
	calls  int
	status int
	body   string
	retry  string
	t      *testing.T
}

func (s *v028OpenCodeHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	s.calls++
	require.True(s.t, HTTPUpstreamRedirectsDisabled(req.Context()))
	require.Equal(s.t, "Bearer test-key", req.Header.Get("Authorization"))
	require.Equal(s.t, openCodeGoOfficialUsageURL, req.URL.String())
	return &http.Response{StatusCode: s.status, Header: http.Header{"Retry-After": []string{s.retry}}, Body: io.NopCloser(strings.NewReader(s.body))}, nil
}

type v028OpenCodeAccounts struct {
	AccountRepository
	accounts map[int64]*Account
	cached   *OpenCodeGoUsageSnapshot
	scope    string
	saves    int
	reject   bool
	dueCalls int
}

func (r *v028OpenCodeAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	return r.accounts[id], nil
}
func (r *v028OpenCodeAccounts) FindRecentOpenCodeGoUsage(_ context.Context, scope string, _ int64) (*OpenCodeGoUsageSnapshot, error) {
	if r.scope == scope {
		return r.cached, nil
	}
	return nil, nil
}
func (r *v028OpenCodeAccounts) SaveOpenCodeGoUsageSnapshot(_ context.Context, a *Account, scope string, snapshot *OpenCodeGoUsageSnapshot) (bool, error) {
	if r.reject {
		return false, nil
	}
	r.saves++
	r.scope = scope
	r.cached = snapshot
	a.Extra[OpenCodeGoUsageSnapshotKey] = snapshot
	a.Extra[OpenCodeGoUsageScopeKey] = scope
	a.Extra[OpenCodeGoUsageIdentityKey] = OpenCodeGoUsageIdentity(a)
	return true, nil
}
func (r *v028OpenCodeAccounts) ConfigureOpenCodeGoUsage(context.Context, *Account, bool, string) (bool, error) {
	return true, nil
}
func (r *v028OpenCodeAccounts) ListDueOpenCodeGoUsageAccounts(context.Context, time.Time, time.Duration, time.Duration, int) ([]*Account, error) {
	r.dueCalls++
	return nil, nil
}
func v028OpenCodeAccount(id int64) *Account {
	return &Account{ID: id, Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}, Extra: map[string]any{cnBillingModeExtraKey: CNBillingModeCodingPlan}}
}

func TestV028OpenCodeEligibilityAndScope(t *testing.T) {
	for _, base := range []string{"https://opencode.ai/zen/go", "https://opencode.ai/zen/go/v1", "https://opencode.ai/zen/go/anthropic/"} {
		a := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": base}}
		require.True(t, IsOpenCodeGoUsageAccount(a))
	}
	for _, base := range []string{"https://opencode.ai.evil.test/zen/go", "https://user:pass@opencode.ai/zen/go", "https://opencode.ai:443/zen/go", "http://opencode.ai/zen/go", "https://opencode.ai/zen", "https://opencode.ai/zen/go/evil", "https://opencode.ai/zen/go?x=1"} {
		require.False(t, isOfficialOpenCodeGoBase(base), base)
	}
	native := v028OpenCodeAccount(1)
	native.Extra[cnBillingModeExtraKey] = CNBillingModePayG
	require.False(t, IsOpenCodeGoUsageAccount(native))
	native.Extra[cnBillingModeExtraKey] = CNBillingModeCodingPlan
	native.Credentials["base_url"] = "https://proxy.example/custom/v1"
	require.True(t, IsOpenCodeGoUsageAccount(native))
	target, err := openCodeGoUsageURL(native)
	require.NoError(t, err)
	require.Equal(t, "https://proxy.example/custom/usage", target)
	native.Extra[OpenCodeGoUsageSourceKey] = "official"
	target, err = openCodeGoUsageURL(native)
	require.NoError(t, err)
	require.Equal(t, openCodeGoOfficialUsageURL, target)
	native.Credentials["account_mode"] = "zen"
	require.False(t, IsOpenCodeGoUsageAccount(native))
}
func TestV028OpenCodeUsageTargetsAcrossPlatformsAndProtocols(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax} {
		t.Run(platform, func(t *testing.T) {
			a := &Account{Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://opencode.ai/zen/go/anthropic/"}}
			target, err := openCodeGoUsageURL(a)
			require.NoError(t, err)
			require.Equal(t, openCodeGoOfficialUsageURL, target)
			a.Credentials["base_url"] = "https://third-party.example/v1"
			a.Extra = map[string]any{OpenCodeGoUsageSourceKey: "official"}
			_, err = openCodeGoUsageURL(a)
			require.Error(t, err, "a source flag must not bypass eligibility")
		})
	}
	for _, protocol := range []string{APIProtocolAdaptive, APIProtocolChatCompletions, APIProtocolAnthropic, APIProtocolResponses} {
		t.Run(protocol, func(t *testing.T) {
			a := v028OpenCodeAccount(1)
			a.Credentials["api_protocol"] = protocol
			a.Credentials["base_url"] = "https://third-party.example/custom/v1"
			target, err := openCodeGoUsageURL(a)
			require.NoError(t, err)
			require.Equal(t, "https://third-party.example/custom/usage", target)
			require.Equal(t, target, openCodeGoConfiguredUsageURL(a), "legacy quota probes must honor the same configured protocol")
			a.Extra[OpenCodeGoUsageSourceKey] = "official"
			target, err = openCodeGoUsageURL(a)
			require.NoError(t, err)
			require.Equal(t, openCodeGoOfficialUsageURL, target)
		})
	}
}

func TestV028OpenCodeDisabledNoActivityOrWorker(t *testing.T) {
	old := openCodeGoUsageAutoEnabled.Load()
	t.Cleanup(func() { openCodeGoUsageAutoEnabled.Store(old) })
	repo := &v028OpenCodeAccounts{}
	svc := &OpenCodeGoUsageService{settings: newMockSettingRepo(), accounts: repo}
	svc.Tick(context.Background())
	defer svc.Stop()
	require.Nil(t, svc.cancel)
	require.Zero(t, repo.dueCalls)
	a := v028OpenCodeAccount(1)
	a.Extra[OpenCodeGoUsageAutoKey] = true
	deferred := &DeferredService{}
	scheduleOpenCodeGoUsageActivity(deferred, a)
	_, ok := deferred.lastUsedUpdates.Load(int64(1))
	require.False(t, ok)
	openCodeGoUsageAutoEnabled.Store(true)
	scheduleOpenCodeGoUsageActivity(deferred, a)
	_, ok = deferred.lastUsedUpdates.Load(int64(1))
	require.True(t, ok)
}
func TestV028OpenCodeSharedFetchAndRateLimit(t *testing.T) {
	a, b := v028OpenCodeAccount(1), v028OpenCodeAccount(2)
	repo := &v028OpenCodeAccounts{accounts: map[int64]*Account{1: a, 2: b}}
	upstream := &v028OpenCodeHTTP{t: t, status: 200, body: `{"usage":{"rolling":{"percent":15,"resetsAt":"2026-09-24T12:00:00Z"},"weekly":{"percent":45}}}`}
	quota := NewCNProviderQuotaService(repo, nil, upstream, nil)
	svc := &OpenCodeGoUsageService{accounts: repo, quota: quota, lock: &leaderLockCacheStub{acquired: true}}
	_, err := svc.Refresh(context.Background(), 1)
	require.NoError(t, err)
	second, err := svc.Refresh(context.Background(), 2)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, 2, repo.saves)
	require.Len(t, second.Snapshot.Tiers, 2)
	_, err = svc.Refresh(context.Background(), 1)
	require.ErrorIs(t, err, ErrOpenCodeGoUsageBusy)
	require.NotContains(t, a.Extra, "opencode_go_5h_used_percent", "new display fetch must not alter scheduler thresholds")
	a.Credentials["api_key"] = "changed"
	require.Nil(t, OpenCodeGoUsageSnapshotForAccount(a))
}
func TestV028OpenCodeFetchBoundsAndBackoff(t *testing.T) {
	a := v028OpenCodeAccount(1)
	upstream := &v028OpenCodeHTTP{t: t, status: 200, body: strings.Repeat(" ", cnQuotaMaxBodyBytes+1)}
	quota := NewCNProviderQuotaService(nil, nil, upstream, nil)
	_, err := quota.fetchOpenCodeGoUsage(context.Background(), a, openCodeGoOfficialUsageURL)
	require.ErrorContains(t, err, "too large")
	upstream.status = 429
	upstream.body = `{"api_key":"must not appear in errors"}`
	upstream.retry = "300"
	repo := &v028OpenCodeAccounts{accounts: map[int64]*Account{1: a}}
	svc := &OpenCodeGoUsageService{accounts: repo, quota: quota, lock: &leaderLockCacheStub{acquired: true}}
	state, err := svc.Refresh(context.Background(), 1)
	require.NoError(t, err)
	require.NotContains(t, state.Snapshot.Error, "must not appear")
	require.GreaterOrEqual(t, state.Snapshot.NextRefreshAt, time.Now().Add(299*time.Second).Unix())
}

func TestV028OpenCodeMissingKeyNeverQueriesUpstream(t *testing.T) {
	a := v028OpenCodeAccount(1)
	a.Credentials["api_key"] = "  "
	upstream := &v028OpenCodeHTTP{t: t}
	quota := NewCNProviderQuotaService(nil, nil, upstream, nil)
	_, err := quota.queryUsageForAccount(context.Background(), a)
	require.ErrorContains(t, err, "account api_key is empty")
	require.Zero(t, upstream.calls)
}
