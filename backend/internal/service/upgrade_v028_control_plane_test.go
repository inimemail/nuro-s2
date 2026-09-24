//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type v028BrokenSettings struct{ SettingRepository }

type v028TransactionalSettings struct{ *mockSettingRepo }

func (r v028TransactionalSettings) WithBackupRecordLock(ctx context.Context, fn func(context.Context) error) error {
	before, err := r.GetAll(ctx)
	if err != nil {
		return err
	}
	if err := fn(ctx); err != nil {
		r.mu.Lock()
		r.data = before
		r.mu.Unlock()
		return err
	}
	return nil
}

type v028PartialDeleteStore struct{ *mockObjectStore }

func (s v028PartialDeleteStore) Delete(ctx context.Context, key string) error {
	if key == "failed.sql.gz" {
		return errors.New("object deletion unavailable")
	}
	return s.mockObjectStore.Delete(ctx, key)
}

func TestV028BackupCleanupPartialFailureCommitsSuccessfulRemovals(t *testing.T) {
	repo := newMockSettingRepo()
	seedS3Config(t, repo)
	store := newMockObjectStore()
	store.objects["deleted.sql.gz"] = []byte("old")
	store.objects["failed.sql.gz"] = []byte("retry")
	svc := newTestBackupService(repo, &mockDumper{}, store)
	svc.settingRepo = v028TransactionalSettings{repo}
	svc.storeFactory = func(context.Context, *BackupS3Config) (BackupObjectStore, error) {
		return v028PartialDeleteStore{store}, nil
	}
	old := time.Now().AddDate(0, 0, -30).Format(time.RFC3339)
	seedBackupRecords(t, svc, BackupRecord{ID: "deleted", Status: "completed", S3Key: "deleted.sql.gz", StartedAt: old}, BackupRecord{ID: "retry", Status: "completed", S3Key: "failed.sql.gz", StartedAt: old})
	require.ErrorContains(t, svc.cleanupOldBackups(context.Background(), &BackupScheduleConfig{RetainDays: 7}), "object deletion unavailable")
	records, err := svc.loadRecords(context.Background())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "retry", records[0].ID)
	require.Contains(t, records[0].ErrorMsg, "object deletion unavailable")
	require.NotContains(t, store.objects, "deleted.sql.gz")
	require.Contains(t, store.objects, "failed.sql.gz")
}

func TestV028BackupRefreshRecoversReservationsAfterStartup(t *testing.T) {
	repo := newMockSettingRepo()
	svc := newTestBackupService(repo, &mockDumper{}, newMockObjectStore())
	seedBackupRecords(t, svc, BackupRecord{ID: "orphan", Status: "running", StartedAt: time.Now().Format(time.RFC3339)})
	svc.recoverStaleRecords()
	record, err := svc.GetBackupRecord(context.Background(), "orphan")
	require.NoError(t, err)
	require.Equal(t, "running", record.Status)
	record.StartedAt = time.Now().Add(-time.Hour).Format(time.RFC3339)
	seedBackupRecords(t, svc, *record, BackupRecord{ID: "restore", Status: "completed", RestoreStatus: "running", RestoreStartedAt: record.StartedAt})
	svc.RefreshSchedule(context.Background())
	record, err = svc.GetBackupRecord(context.Background(), "orphan")
	require.NoError(t, err)
	require.Equal(t, "failed", record.Status)
	record, err = svc.GetBackupRecord(context.Background(), "restore")
	require.NoError(t, err)
	require.Equal(t, "failed", record.RestoreStatus)
}

func TestV028BackupIDsRetainFullUUID(t *testing.T) {
	for _, async := range []bool{false, true} {
		repo := newMockSettingRepo()
		seedS3Config(t, repo)
		svc := newTestBackupService(repo, &mockDumper{dumpData: []byte("database")}, newMockObjectStore())
		create := svc.CreateBackup
		if async {
			create = svc.StartBackup
		}
		record, err := create(context.Background(), "manual", 0)
		require.NoError(t, err)
		svc.wg.Wait()
		require.NoError(t, uuid.Validate(record.ID))
		require.Len(t, record.ID, 36)
		require.Contains(t, record.FileName, record.ID)
	}
}

type v028WriteFailSettings struct{ SettingRepository }

func (v028WriteFailSettings) Set(context.Context, string, string) error {
	return errors.New("write failed")
}

type v028UncertainCompletionSettings struct{ SettingRepository }

func (r v028UncertainCompletionSettings) Set(ctx context.Context, key, value string) error {
	if err := r.SettingRepository.Set(ctx, key, value); err != nil {
		return err
	}
	if key == settingKeyBackupRecords {
		var records []BackupRecord
		if json.Unmarshal([]byte(value), &records) == nil && len(records) > 0 && records[0].Status == "completed" {
			return errors.New("commit acknowledgement lost")
		}
	}
	return nil
}

func TestV028UncertainCompletedCommitNeverDeletesUploadedBackup(t *testing.T) {
	repo := newMockSettingRepo()
	seedS3Config(t, repo)
	store := newMockObjectStore()
	svc := newTestBackupService(repo, &mockDumper{dumpData: []byte("database")}, store)
	svc.settingRepo = v028UncertainCompletionSettings{repo}
	record, err := svc.CreateBackup(context.Background(), "manual", 0)
	require.ErrorContains(t, err, "commit acknowledgement lost")
	stored, err := svc.GetBackupRecord(context.Background(), record.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", stored.Status)
	require.Contains(t, store.objects, stored.S3Key)
	require.Contains(t, stored.FileName, stored.ID)
}

func TestV028StaleBackupSaveFailureKeepsObjects(t *testing.T) {
	repo := newMockSettingRepo()
	seedS3Config(t, repo)
	store := newMockObjectStore()
	store.objects["protected.sql.gz"] = []byte("archive")
	svc := newTestBackupService(repo, &mockDumper{}, store)
	seedBackupRecords(t, svc, BackupRecord{ID: "old", Status: "running", S3Key: "protected.sql.gz", StartedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)})
	svc.settingRepo = v028WriteFailSettings{repo}
	svc.recoverStaleRecords()
	require.Contains(t, store.objects, "protected.sql.gz")
	record, err := svc.GetBackupRecord(context.Background(), "old")
	require.NoError(t, err)
	require.Equal(t, "running", record.Status)
}

func (v028BrokenSettings) GetValue(context.Context, string) (string, error) {
	return "", errors.New("storage down")
}
func (v028BrokenSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, errors.New("storage down")
}

func TestV028ArchiveCalendar(t *testing.T) {
	cfg := &BackupMonthlyArchiveConfig{Enabled: true, Days: []int{31, 1, 31, 15}, IncludeMonthEnd: true}
	require.NoError(t, normalizeBackupMonthlyArchive(cfg))
	require.Equal(t, []int{1, 15, 31}, cfg.Days)
	require.Equal(t, []string{"2028-02-15", "2028-02-29"}, backupArchiveDates(time.Date(2028, 2, 29, 23, 0, 0, 0, time.UTC), cfg, "2028-02-01"))
	require.Equal(t, []string{"2026-03-01"}, backupArchiveDates(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), cfg, "2026-01-31"))
	require.Empty(t, backupArchiveDates(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), cfg, "2026-03-01"))
	cfg.Enabled = false
	require.Empty(t, backupArchiveDates(time.Now(), cfg, ""))
}

func TestV028BackupCallbackFencingAndRestoreRollback(t *testing.T) {
	ctx := context.Background()
	repo := newMockSettingRepo()
	svc := newTestBackupService(repo, &mockDumper{}, newMockObjectStore())
	seedBackupRecords(t, svc, BackupRecord{ID: "a", Status: "completed", MonthlyArchive: &BackupMonthlyArchive{Dates: []string{"2026-09-01"}, RetainCount: 0}}, BackupRecord{ID: "b", Status: "completed"})
	require.NoError(t, repo.Set(ctx, backupArchiveCheckpointKey, `"2026-09-01"`))
	reservation, err := svc.reserveRestore(ctx, "a")
	require.NoError(t, err)
	_, err = svc.reserveRestore(ctx, "b")
	require.Error(t, err)
	require.Error(t, svc.saveRecord(ctx, &BackupRecord{ID: "missing", Status: "completed"}))
	// Restoring SQL rolls all settings back, including the active reservation.
	require.NoError(t, repo.Set(ctx, settingKeyBackupRecords, `[]`))
	require.NoError(t, repo.Set(ctx, backupArchiveCheckpointKey, `"2026-08-01"`))
	require.NoError(t, svc.finishRestore(reservation, nil))
	records, err := svc.loadRecords(ctx)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "completed", records[0].RestoreStatus)
	require.Zero(t, records[0].MonthlyArchive.RetainCount)
	checkpoint, err := repo.GetValue(ctx, backupArchiveCheckpointKey)
	require.NoError(t, err)
	require.Equal(t, `"2026-09-01"`, checkpoint)
	stale := *reservation.record
	stale.RestoreStatus = "running"
	require.Error(t, svc.saveRecord(ctx, &stale))
}

func TestV028BackupReadFailureDoesNotBecomeEmpty(t *testing.T) {
	svc := &BackupService{settingRepo: v028BrokenSettings{}}
	_, err := svc.loadRecords(context.Background())
	require.Error(t, err)
	require.Error(t, svc.saveRecord(context.Background(), &BackupRecord{ID: "a", Status: "running"}))
}

func TestV028RequestRetentionDefaultsAndInvalidSettings(t *testing.T) {
	repo := newMockSettingRepo()
	svc := &DashboardAggregationService{settingRepo: repo, cfg: config.DashboardAggregationConfig{Retention: config.DashboardAggregationRetentionConfig{UsageLogsDays: 30}}}
	days, override, err := svc.requestRetention(context.Background())
	require.NoError(t, err)
	require.Equal(t, 30, days)
	require.False(t, override)
	require.NoError(t, repo.Set(context.Background(), SettingKeyOpsRuntimeLogConfig, `{"request_retention_override_enabled":true,"request_retention_days":0}`))
	days, override, err = svc.requestRetention(context.Background())
	require.NoError(t, err)
	require.Zero(t, days)
	require.True(t, override)
	require.NoError(t, repo.Set(context.Background(), SettingKeyOpsRuntimeLogConfig, `{"request_retention_override_enabled":true}`))
	_, _, err = svc.requestRetention(context.Background())
	require.Error(t, err)
	svc.settingRepo = v028BrokenSettings{}
	_, _, err = svc.requestRetention(context.Background())
	require.Error(t, err)
}

func TestV028ClaudeVersionPriorityAndDisabledNoNetwork(t *testing.T) {
	previous := claudeCLIIdentitySnapshot.Load()
	t.Cleanup(func() { claudeCLIIdentitySnapshot.Store(previous) })
	repo := newMockSettingRepo()
	github := &codexVersionSyncGitHubStub{}
	svc := &ClaudeCLIVersionSyncService{repo: repo, github: github}
	defer svc.Stop()
	svc.Refresh(context.Background())
	require.Zero(t, github.latestCalls)
	require.Zero(t, github.recentCalls)
	require.Nil(t, svc.cancel)
	svc.Apply("2.2.0", "2.3.0", true)
	require.Equal(t, "2.2.0", currentClaudeCLIIdentity().version)
	ctx := captureClaudeCLIIdentity(context.Background())
	svc.Apply("", "2.3.0", true)
	svc.Apply("", "2.2.0", true)
	require.Equal(t, "2.3.0", currentClaudeCLIIdentity().version)
	svc.Apply("", "", true)
	require.Equal(t, "2.3.0", currentClaudeCLIIdentity().version)
	require.Equal(t, "2.2.0", claudeCLIVersionForContext(ctx))
	svc.Apply("", "2.3.0", false)
	require.Equal(t, "builtin", currentClaudeCLIIdentity().source)
	svc.Apply("2.2.0", "", false)
	svc.repo = v028BrokenSettings{}
	svc.Refresh(context.Background())
	require.Equal(t, "2.2.0", currentClaudeCLIIdentity().version)
	require.Equal(t, "2.4.0", latestClaudeStableVersion([]*GitHubRelease{{TagName: "v2.4.0"}, {TagName: "v9.0.0", Draft: true}, {TagName: "v2.5.0-rc.1", Prerelease: true}}))
}

func TestV028BackupFinalizationCannotOutliveLease(t *testing.T) {
	svc := &BackupService{}
	svc.operationDeadline.Store(time.Now().Add(-time.Second).UnixNano())
	ctx, cancel := svc.boundBackupFinalization(context.Background())
	defer cancel()
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
}

func TestV028RetentionStopCancelsInFlightWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	svc := &DashboardAggregationService{retentionCancel: cancel, retentionScheduled: true}
	svc.StopRetentionSchedule()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.False(t, svc.retentionScheduled)
	require.True(t, svc.retentionStopped)
}

func TestV028DeferredActivityMonotonic(t *testing.T) {
	svc := &DeferredService{}
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); svc.mergeLastUsed(1, now.Add(time.Duration(i)*time.Second)) }(i)
	}
	wg.Wait()
	svc.mergeLastUsed(1, now.Add(-time.Minute))
	actual, _ := svc.lastUsedUpdates.Load(int64(1))
	require.Equal(t, now.Add(49*time.Second), actual)
}
