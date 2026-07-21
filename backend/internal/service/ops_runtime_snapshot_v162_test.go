package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type opsRuntimeRefreshRepo struct {
	SettingRepository
	mu     sync.RWMutex
	values map[string]string
	fail   atomic.Bool
	calls  atomic.Int64
}

func (r *opsRuntimeRefreshRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	r.calls.Add(1)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if r.fail.Load() {
		return nil, errors.New("settings unavailable")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *opsRuntimeRefreshRepo) set(key, value string) {
	r.mu.Lock()
	r.values[key] = value
	r.mu.Unlock()
}

func waitForOpsRefresh(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not satisfied before timeout")
}

func TestOpsRuntimeSettingsSnapshotKeepsRequestPathOffRepository(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	repo.values[SettingKeyOpsMonitoringEnabled] = "false"
	repo.values[SettingKeyOpsAdvancedSettings] = `{"auto_refresh_interval_seconds":45}`
	svc := &OpsService{settingRepo: repo}
	svc.initRuntimeSettings(context.Background())
	if repo.getMultipleCalls != 1 {
		t.Fatalf("startup GetMultiple calls = %d, want 1", repo.getMultipleCalls)
	}
	for range 1000 {
		if svc.IsMonitoringEnabled(context.Background()) {
			t.Fatal("monitoring enabled, want false")
		}
		if svc.OpsAdvancedSettingsSnapshot().AutoRefreshIntervalSec != 45 {
			t.Fatal("advanced settings snapshot changed")
		}
	}
	if repo.getValueCalls != 0 || repo.getMultipleCalls != 1 {
		t.Fatalf("request path touched repository: get=%d get_multiple=%d", repo.getValueCalls, repo.getMultipleCalls)
	}
}

func TestOpsRuntimeSettingsBackgroundRefreshConvergesAndStops(t *testing.T) {
	repo := &opsRuntimeRefreshRepo{values: map[string]string{SettingKeyOpsMonitoringEnabled: "false"}}
	svc := &OpsService{settingRepo: repo}
	svc.initRuntimeSettings(context.Background())
	repo.set(SettingKeyOpsMonitoringEnabled, "true")
	svc.startRuntimeSettingsRefresh(context.Background(), 5*time.Millisecond, 0, 50*time.Millisecond)
	waitForOpsRefresh(t, func() bool {
		return svc.IsMonitoringEnabled(context.Background()) && svc.RuntimeSettingsRefreshHealth().SuccessTotal > 0
	})
	svc.StopRuntimeSettingsRefresh()
	callsAfterStop := repo.calls.Load()
	time.Sleep(20 * time.Millisecond)
	if repo.calls.Load() != callsAfterStop || svc.RuntimeSettingsRefreshHealth().Running {
		t.Fatal("runtime refresh continued after stop")
	}
	svc.StopRuntimeSettingsRefresh()
}

func TestOpsRuntimeSettingsRefreshFailureKeepsLastKnownGood(t *testing.T) {
	repo := &opsRuntimeRefreshRepo{values: map[string]string{
		SettingKeyOpsMonitoringEnabled: "false",
		SettingKeyOpsAdvancedSettings:  `{"ignore_no_available_accounts":true}`,
	}}
	svc := &OpsService{settingRepo: repo}
	svc.initRuntimeSettings(context.Background())
	repo.fail.Store(true)
	svc.startRuntimeSettingsRefresh(context.Background(), 5*time.Millisecond, 0, 50*time.Millisecond)
	t.Cleanup(svc.StopRuntimeSettingsRefresh)
	waitForOpsRefresh(t, func() bool { return svc.RuntimeSettingsRefreshHealth().FailureTotal >= 3 })
	if svc.IsMonitoringEnabled(context.Background()) || !svc.OpsAdvancedSettingsSnapshot().IgnoreNoAvailableAccounts {
		t.Fatal("failed refresh overwrote last known good snapshot")
	}
}
