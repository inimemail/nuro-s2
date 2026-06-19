package service

import (
	"context"
	"net/http"
	"testing"
	"time"
)

type accountUsageOpenAITokenCacheStub struct {
	token string
}

func (s *accountUsageOpenAITokenCacheStub) GetAccessToken(context.Context, string) (string, error) {
	return s.token, nil
}

func (s *accountUsageOpenAITokenCacheStub) SetAccessToken(context.Context, string, string, time.Duration) error {
	return nil
}

func (s *accountUsageOpenAITokenCacheStub) DeleteAccessToken(context.Context, string) error {
	return nil
}

func (s *accountUsageOpenAITokenCacheStub) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

func (s *accountUsageOpenAITokenCacheStub) ReleaseRefreshLock(context.Context, string) error {
	return nil
}

type accountUsageCodexProbeRepo struct {
	stubOpenAIAccountRepo
	updateExtraCh chan map[string]any
	rateLimitCh   chan time.Time
}

func (r *accountUsageCodexProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updateExtraCh != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCh <- copied
	}
	return nil
}

func (r *accountUsageCodexProbeRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	if r.rateLimitCh != nil {
		r.rateLimitCh <- resetAt
	}
	return nil
}

func TestShouldRefreshOpenAICodexSnapshot(t *testing.T) {
	t.Parallel()

	rateLimitedUntil := time.Now().Add(5 * time.Minute)
	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{RateLimitResetAt: &rateLimitedUntil}, usage, now) {
		t.Fatal("expected rate-limited account to force codex snapshot refresh")
	}

	if shouldRefreshOpenAICodexSnapshot(&Account{}, usage, now) {
		t.Fatal("expected complete non-rate-limited usage to skip codex snapshot refresh")
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{}, &UsageInfo{FiveHour: nil, SevenDay: &UsageProgress{}}, now) {
		t.Fatal("expected missing 5h snapshot to require refresh")
	}

	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	if !shouldRefreshOpenAICodexSnapshot(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_enabled": true,
			"codex_usage_updated_at":                       staleAt,
		},
	}, usage, now) {
		t.Fatal("expected stale ws snapshot to trigger refresh")
	}
}

func TestExtractOpenAICodexProbeUpdatesAccepts429WithCodexHeaders(t *testing.T) {
	t.Parallel()

	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", "100")
	headers.Set("x-codex-secondary-reset-after-seconds", "18000")
	headers.Set("x-codex-secondary-window-minutes", "300")

	updates, err := extractOpenAICodexProbeUpdates(&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})
	if err != nil {
		t.Fatalf("extractOpenAICodexProbeUpdates() error = %v", err)
	}
	if len(updates) == 0 {
		t.Fatal("expected codex probe updates from 429 headers")
	}
	if got := updates["codex_5h_used_percent"]; got != 100.0 {
		t.Fatalf("codex_5h_used_percent = %v, want 100", got)
	}
	if got := updates["codex_7d_used_percent"]; got != 100.0 {
		t.Fatalf("codex_7d_used_percent = %v, want 100", got)
	}
}

func TestAccountUsageService_PersistOpenAICodexProbeSnapshotOnlyUpdatesExtra(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	svc.persistOpenAICodexProbeSnapshot(321, map[string]any{
		"codex_7d_used_percent": 100.0,
		"codex_7d_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
	})

	select {
	case updates := <-repo.updateExtraCh:
		if got := updates["codex_7d_used_percent"]; got != 100.0 {
			t.Fatalf("codex_7d_used_percent = %v, want 100", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 codex 探测快照写入 extra 超时")
	}

	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将探测快照写入运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_GetOpenAIUsage_DoesNotPromoteCodexExtraToRateLimit(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	repo := &accountUsageCodexProbeRepo{
		rateLimitCh: make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_5h_used_percent": 1.0,
			"codex_5h_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
			"codex_7d_used_percent": 100.0,
			"codex_7d_reset_at":     resetAt.Format(time.RFC3339),
		},
	}

	usage, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if usage.SevenDay == nil || usage.SevenDay.Utilization != 100.0 {
		t.Fatalf("预期 7 天用量仍然可见，实际为 %#v", usage.SevenDay)
	}
	if account.RateLimitResetAt != nil {
		t.Fatalf("不应让已耗尽的 codex extra 改写运行时限流状态: %v", account.RateLimitResetAt)
	}
	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将已耗尽的 codex extra 持久化为运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestExtractOpenAICodexResetCreditUpdates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	updates, supported, ok := extractOpenAICodexResetCreditUpdates([]byte(`{"rate_limit_reset_credits":{"available_count":2}}`), now)
	if !ok {
		t.Fatal("expected reset credit payload to be recognized")
	}
	if !supported {
		t.Fatal("expected reset credits to be supported")
	}
	if got := updates["codex_reset_credits"]; got != 2 {
		t.Fatalf("codex_reset_credits = %v, want 2", got)
	}
	if got := updates["codex_reset_credits_supported"]; got != true {
		t.Fatalf("codex_reset_credits_supported = %v, want true", got)
	}

	updates, supported, ok = extractOpenAICodexResetCreditUpdates([]byte(`{"rate_limit_reset_credits":{"available_count":1,"invite_url":"https://chatgpt.com/invite/codex-reset"}}`), now)
	if !ok || !supported {
		t.Fatalf("expected supported reset credit payload with ignored invite URL, supported=%v ok=%v", supported, ok)
	}
	if _, exists := updates["codex_reset_credits_invite_url"]; exists {
		t.Fatalf("did not expect unsupported invite URL field to be persisted: %v", updates)
	}

	updates, supported, ok = extractOpenAICodexResetCreditUpdates([]byte(`{"rate_limit_reset_credits":{}}`), now)
	if !ok {
		t.Fatal("expected missing available_count to be cacheable as zero")
	}
	if !supported {
		t.Fatal("expected reset credits to be supported with zero fallback")
	}
	if got := updates["codex_reset_credits"]; got != 0 {
		t.Fatalf("codex_reset_credits = %v, want 0 when available_count is missing", got)
	}
	if got := updates["codex_reset_credits_supported"]; got != true {
		t.Fatalf("codex_reset_credits_supported = %v, want true", got)
	}
	if got := updates["codex_auto_reset_mode"]; got != openAICodexAutoResetModeOff {
		t.Fatalf("codex_auto_reset_mode = %v, want off", got)
	}

	updates, supported, ok = extractOpenAICodexResetCreditUpdates([]byte(`not json`), now)
	if ok {
		t.Fatal("expected invalid response body to be treated as unknown")
	}
	if supported {
		t.Fatal("expected invalid response support to remain unknown")
	}
	if len(updates) != 0 {
		t.Fatalf("expected no updates for invalid response body, got %v", updates)
	}
}

func TestAccountUsageOpenAIWhamAccessTokenUsesProvider(t *testing.T) {
	t.Parallel()

	account := &Account{
		ID:       1234,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "stale-account-token",
		},
	}
	provider := NewOpenAITokenProvider(nil, &accountUsageOpenAITokenCacheStub{token: "fresh-provider-token"}, nil)
	svc := &AccountUsageService{openAITokenProvider: provider}

	token, err := svc.openAIWhamAccessToken(context.Background(), account)
	if err != nil {
		t.Fatalf("openAIWhamAccessToken() error = %v", err)
	}
	if token != "fresh-provider-token" {
		t.Fatalf("token = %q, want provider token", token)
	}
}

func TestBuildOpenAIUsageFromExtraIncludesResetCredits(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	usage := buildOpenAIUsageFromExtra(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_reset_credits": 0,
		},
	}, now)

	if !usage.CodexResetCreditsSupported {
		t.Fatal("expected reset credits to be supported when count exists in extra")
	}
	if usage.CodexResetCreditsAvailableCount == nil || *usage.CodexResetCreditsAvailableCount != 0 {
		t.Fatalf("available count = %v, want 0", usage.CodexResetCreditsAvailableCount)
	}
	if usage.CodexAutoResetMode != openAICodexAutoResetModeOff {
		t.Fatalf("auto reset mode = %q, want off when count is 0", usage.CodexAutoResetMode)
	}
}

func TestNormalizeOpenAICodexAutoResetModeAliases(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"":       openAICodexAutoResetModeOff,
		"off":    openAICodexAutoResetModeOff,
		"short":  openAICodexAutoResetModeShort,
		"5h":     openAICodexAutoResetModeShort,
		"hour":   openAICodexAutoResetModeShort,
		"long":   openAICodexAutoResetModeLong,
		"7d":     openAICodexAutoResetModeLong,
		"week":   openAICodexAutoResetModeLong,
		"month":  openAICodexAutoResetModeLong,
		"random": openAICodexAutoResetModeOff,
	}
	for raw, want := range tests {
		if got := normalizeOpenAICodexAutoResetMode(raw); got != want {
			t.Fatalf("normalize(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSetOpenAICodexAutoResetModeNilExtraFallsBackOff(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{{
			ID:       123,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    nil,
		}}},
		updateExtraCh: make(chan map[string]any, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}

	usage, err := svc.SetOpenAICodexAutoResetMode(context.Background(), 123, openAICodexAutoResetModeShort)
	if err != nil {
		t.Fatalf("SetOpenAICodexAutoResetMode returned error: %v", err)
	}
	if usage.CodexAutoResetMode != openAICodexAutoResetModeOff {
		t.Fatalf("auto reset mode = %q, want off", usage.CodexAutoResetMode)
	}
	select {
	case updates := <-repo.updateExtraCh:
		if updates["codex_auto_reset_mode"] != openAICodexAutoResetModeOff {
			t.Fatalf("persisted mode = %v, want off", updates["codex_auto_reset_mode"])
		}
	case <-time.After(time.Second):
		t.Fatal("expected UpdateExtra to be called")
	}
}

func TestApplyOpenAICodexResetCreditsFromExtraHonorsUnsupportedFlag(t *testing.T) {
	t.Parallel()

	usage := &UsageInfo{}
	applyOpenAICodexResetCreditsFromExtra(usage, map[string]any{
		"codex_reset_credits_supported": false,
		"codex_reset_credits":           3,
	})
	if usage.CodexResetCreditsSupported {
		t.Fatal("expected unsupported flag to block display")
	}
	if usage.CodexResetCreditsAvailableCount != nil {
		t.Fatalf("expected nil count, got %v", usage.CodexResetCreditsAvailableCount)
	}
}

func TestBuildOpenAICodexResetCreditOptimisticUpdates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	updates := buildOpenAICodexResetCreditOptimisticUpdates(&Account{
		Extra: map[string]any{
			"codex_reset_credits_supported": true,
			"codex_reset_credits":           2,
		},
	}, now)
	if got := updates["codex_reset_credits"]; got != 1 {
		t.Fatalf("codex_reset_credits = %v, want 1", got)
	}
	if got := updates["codex_reset_credits_supported"]; got != true {
		t.Fatalf("codex_reset_credits_supported = %v, want true", got)
	}
	if got := updates["codex_reset_credits_updated_at"]; got != now.Format(time.RFC3339) {
		t.Fatalf("updated_at = %v, want %s", got, now.Format(time.RFC3339))
	}

	updates = buildOpenAICodexResetCreditOptimisticUpdates(&Account{
		Extra: map[string]any{
			"codex_reset_credits_supported": false,
			"codex_reset_credits":           2,
		},
	}, now)
	if len(updates) != 0 {
		t.Fatalf("expected no optimistic update for unsupported account, got %v", updates)
	}
}

func TestOpenAICodexResetCreditUpdatesConfirmed(t *testing.T) {
	t.Parallel()

	if openAICodexResetCreditUpdatesConfirmed(nil) {
		t.Fatal("nil updates should not be confirmed")
	}
	if openAICodexResetCreditUpdatesConfirmed(map[string]any{"codex_5h_used_percent": 100}) {
		t.Fatal("usage-only updates should not be confirmed reset credit updates")
	}
	if !openAICodexResetCreditUpdatesConfirmed(map[string]any{"codex_reset_credits": 0}) {
		t.Fatal("presence of codex_reset_credits should confirm reset credit query")
	}
}

func TestBuildCodexUsageProgressFromExtra_ZerosExpiredWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)

	t.Run("expired 5h window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     "2026-03-16T10:00:00Z", // 2h ago
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired window, got %v", progress.Utilization)
		}
		if progress.RemainingSeconds != 0 {
			t.Fatalf("expected RemainingSeconds=0, got %v", progress.RemainingSeconds)
		}
	})

	t.Run("active 5h window keeps utilization", func(t *testing.T) {
		resetAt := now.Add(2 * time.Hour).Format(time.RFC3339)
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     resetAt,
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 42.0 {
			t.Fatalf("expected Utilization=42, got %v", progress.Utilization)
		}
	})

	t.Run("expired 7d window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_7d_used_percent": 88.0,
			"codex_7d_reset_at":     "2026-03-15T00:00:00Z", // yesterday
		}
		progress := buildCodexUsageProgressFromExtra(extra, "7d", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired 7d window, got %v", progress.Utilization)
		}
	})
}
