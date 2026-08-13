package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestGetOpsAdvancedSettings_DefaultHidesOpenAITokenStats(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	svc := &OpsService{settingRepo: repo}

	cfg, err := svc.GetOpsAdvancedSettings(context.Background())
	if err != nil {
		t.Fatalf("GetOpsAdvancedSettings() error = %v", err)
	}
	if cfg.DisplayOpenAITokenStats {
		t.Fatalf("DisplayOpenAITokenStats = true, want false by default")
	}
	if !cfg.DisplayAlertEvents {
		t.Fatalf("DisplayAlertEvents = false, want true by default")
	}
	if repo.setCalls != 0 {
		t.Fatalf("hot-path snapshot must not persist defaults, got %d writes", repo.setCalls)
	}
}

func TestUpdateOpsAdvancedSettings_PersistsOpenAITokenStatsVisibility(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	svc := &OpsService{settingRepo: repo}

	cfg := defaultOpsAdvancedSettings()
	cfg.DisplayOpenAITokenStats = true
	cfg.DisplayAlertEvents = false

	updated, err := svc.UpdateOpsAdvancedSettings(context.Background(), cfg)
	if err != nil {
		t.Fatalf("UpdateOpsAdvancedSettings() error = %v", err)
	}
	if !updated.DisplayOpenAITokenStats {
		t.Fatalf("DisplayOpenAITokenStats = false, want true")
	}
	if updated.DisplayAlertEvents {
		t.Fatalf("DisplayAlertEvents = true, want false")
	}

	reloaded, err := svc.GetOpsAdvancedSettings(context.Background())
	if err != nil {
		t.Fatalf("GetOpsAdvancedSettings() after update error = %v", err)
	}
	if !reloaded.DisplayOpenAITokenStats {
		t.Fatalf("reloaded DisplayOpenAITokenStats = false, want true")
	}
	if reloaded.DisplayAlertEvents {
		t.Fatalf("reloaded DisplayAlertEvents = true, want false")
	}
}

func TestGetOpsAdvancedSettings_BackfillsNewDisplayFlagsFromDefaults(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	svc := &OpsService{settingRepo: repo}

	legacyCfg := map[string]any{
		"data_retention": map[string]any{
			"cleanup_enabled":               false,
			"cleanup_schedule":              "0 2 * * *",
			"error_log_retention_days":      30,
			"minute_metrics_retention_days": 30,
			"hourly_metrics_retention_days": 30,
		},
		"aggregation": map[string]any{
			"aggregation_enabled": false,
		},
		"ignore_count_tokens_errors":    true,
		"ignore_context_canceled":       true,
		"ignore_no_available_accounts":  false,
		"ignore_invalid_api_key_errors": false,
		"auto_refresh_enabled":          false,
		"auto_refresh_interval_seconds": 30,
	}
	raw, err := json.Marshal(legacyCfg)
	if err != nil {
		t.Fatalf("marshal legacy config: %v", err)
	}
	repo.values[SettingKeyOpsAdvancedSettings] = string(raw)

	cfg, err := svc.GetOpsAdvancedSettings(context.Background())
	if err != nil {
		t.Fatalf("GetOpsAdvancedSettings() error = %v", err)
	}
	if cfg.DisplayOpenAITokenStats {
		t.Fatalf("DisplayOpenAITokenStats = true, want false default backfill")
	}
	if !cfg.DisplayAlertEvents {
		t.Fatalf("DisplayAlertEvents = false, want true default backfill")
	}
}

func TestGetOpenAIQuotaAutoPauseSettings_ReadsDefaultsFromOpsAdvancedSettings(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	repo.values[SettingKeyOpsAdvancedSettings] = `{"openai_account_quota_auto_pause":{"default_threshold_5h":0.95,"default_threshold_7d":0.9}}`
	svc := NewSettingService(repo, &config.Config{})

	// Warm the in-memory cache synchronously so the assertion below is deterministic.
	// GetOpenAIQuotaAutoPauseSettings is non-blocking on the hot path (returns the
	// cached value, refreshes asynchronously); for tests and startup, Warm is the
	// synchronous entry point that guarantees a populated cache.
	settings := svc.WarmOpenAIQuotaAutoPauseSettings(context.Background())
	if settings.DefaultThreshold5h != 0.95 {
		t.Fatalf("DefaultThreshold5h = %v, want 0.95", settings.DefaultThreshold5h)
	}
	if settings.DefaultThreshold7d != 0.9 {
		t.Fatalf("DefaultThreshold7d = %v, want 0.9", settings.DefaultThreshold7d)
	}

	// Subsequent Get must hit the warm cache and return the same value without any DB
	// access — that's the hot-path invariant.
	cached := svc.GetOpenAIQuotaAutoPauseSettings(context.Background())
	if cached.DefaultThreshold5h != 0.95 || cached.DefaultThreshold7d != 0.9 {
		t.Fatalf("cached read = %+v, want {0.95, 0.9}", cached)
	}
}

func TestWarmOpenAIQuotaAutoPauseSettings_MissingSettingCachesExplicitDefaults(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	svc := NewSettingService(repo, &config.Config{})

	first := svc.WarmOpenAIQuotaAutoPauseSettings(context.Background())
	if first.DefaultThreshold5h != 0 || first.DefaultThreshold7d != 0 {
		t.Fatalf("missing setting = %+v, want explicit disabled defaults", first)
	}
	if repo.getValueCalls != 1 {
		t.Fatalf("repository reads after warm = %d, want 1", repo.getValueCalls)
	}

	second := svc.GetOpenAIQuotaAutoPauseSettings(context.Background())
	if second != first {
		t.Fatalf("cached missing setting = %+v, want %+v", second, first)
	}
	if repo.getValueCalls != 1 {
		t.Fatalf("cached missing setting repeated repository read: got %d, want 1", repo.getValueCalls)
	}

	cached, _ := svc.openAIQuotaAutoPauseSettingsCache.Load().(*cachedOpenAIQuotaAutoPauseSettings)
	if cached == nil {
		t.Fatal("missing setting did not create an explicit cache snapshot")
	}
	minimumExpiry := time.Now().Add(openAIQuotaAutoPauseSettingsCacheTTL - time.Second).UnixNano()
	if cached.expiresAt < minimumExpiry {
		t.Fatalf("missing setting cache expiry = %d, want at least %d", cached.expiresAt, minimumExpiry)
	}
}

// Hot-path invariant: a Get with cold cache must return immediately (zero defaults)
// rather than blocking on the DB. The async refresher will populate the cache for
// subsequent calls.
func TestGetOpenAIQuotaAutoPauseSettings_ColdCacheNonBlocking(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	repo.values[SettingKeyOpsAdvancedSettings] = `{"openai_account_quota_auto_pause":{"default_threshold_5h":0.7}}`
	svc := NewSettingService(repo, &config.Config{})

	start := time.Now()
	settings := svc.GetOpenAIQuotaAutoPauseSettings(context.Background())
	elapsed := time.Since(start)
	if elapsed > 50*time.Millisecond {
		t.Fatalf("cold-cache Get must be non-blocking, took %v", elapsed)
	}
	// Cold cache means we get zero defaults (the async refresh hasn't completed yet).
	if settings.DefaultThreshold5h != 0 || settings.DefaultThreshold7d != 0 {
		t.Fatalf("cold-cache Get = %+v, want zeroes", settings)
	}
}

// Explicit cache write (e.g. from UpdateOpsAdvancedSettings) must be visible on the
// very next read without any DB roundtrip.
func TestSetOpenAIQuotaAutoPauseSettings_VisibleImmediately(t *testing.T) {
	svc := NewSettingService(newRuntimeSettingRepoStub(), &config.Config{})

	svc.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{
		DefaultThreshold5h: 0.88,
		DefaultThreshold7d: 0.77,
	})

	got := svc.GetOpenAIQuotaAutoPauseSettings(context.Background())
	if got.DefaultThreshold5h != 0.88 || got.DefaultThreshold7d != 0.77 {
		t.Fatalf("after Set, Get = %+v, want {0.88, 0.77}", got)
	}
}

func TestOpenAICodexRoutingHintRuntime_IsCachedAndDefaultOff(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	svc := NewSettingService(repo, &config.Config{})

	if err := svc.RefreshOpenAICodexRoutingHintRuntime(context.Background()); err != nil {
		t.Fatalf("refresh missing hint setting: %v", err)
	}
	if svc.IsOpenAICodexRoutingHintEnabled(context.Background()) {
		t.Fatal("missing routing hint setting must default to disabled")
	}
	if repo.getValueCalls != 1 {
		t.Fatalf("refresh reads = %d, want 1", repo.getValueCalls)
	}
	for range 3 {
		if svc.IsOpenAICodexRoutingHintEnabled(context.Background()) {
			t.Fatal("cached default unexpectedly enabled")
		}
	}
	if repo.getValueCalls != 1 {
		t.Fatalf("hot-path reads = %d, want 1", repo.getValueCalls)
	}

	repo.values[SettingKeyOpenAICodexRoutingHintEnabled] = "true"
	if err := svc.RefreshOpenAICodexRoutingHintRuntime(context.Background()); err != nil {
		t.Fatalf("refresh enabled hint setting: %v", err)
	}
	if !svc.IsOpenAICodexRoutingHintEnabled(context.Background()) {
		t.Fatal("refreshed routing hint setting did not become enabled")
	}
}
