package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// All backup object writers and restores share this lock. Work has a 30-minute
// deadline, with five minutes reserved for cancellation and metadata completion.
// A Redis outage never switches the operation to a different lock backend.
func (s *BackupService) acquireBackupOperation(ctx context.Context) (context.Context, func(), bool) {
	if !s.operationMu.TryLock() {
		return nil, nil, false
	}
	started := time.Now()
	bounded, cancel := context.WithTimeout(ctx, 30*time.Minute)
	release := func() {}
	if s.lockCache != nil || s.db != nil {
		var ok bool
		release, ok = tryAcquireFixedLeaderLock(bounded, s.lockCache, s.db, "backup:operation", uuid.NewString(), 35*time.Minute)
		if !ok {
			cancel()
			s.operationMu.Unlock()
			return nil, nil, false
		}
	}
	s.operationDeadline.Store(started.Add(34 * time.Minute).UnixNano())
	var once sync.Once
	return bounded, func() { once.Do(func() { cancel(); release(); s.operationDeadline.Store(0); s.operationMu.Unlock() }) }, true
}

type backupRestoreReservation struct {
	record     *BackupRecord
	records    []BackupRecord
	checkpoint string
}

func (s *BackupService) reserveRestore(ctx context.Context, id string) (*backupRestoreReservation, error) {
	reservation := &backupRestoreReservation{}
	err := s.withBackupRecordLock(ctx, func(txCtx context.Context) error {
		records, err := s.loadRecordsLocked(txCtx)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.Status == "running" || (record.RestoreStatus == "running" && !backupOperationStale(record.RestoreStartedAt)) {
				return ErrBackupInProgress
			}
		}
		for i := range records {
			if records[i].ID == id {
				if records[i].Status != "completed" {
					return fmt.Errorf("can only restore from a completed backup")
				}
				records[i].RestoreStatus = "running"
				records[i].RestoreOperationID = uuid.NewString()
				records[i].RestoreStartedAt = time.Now().UTC().Format(time.RFC3339Nano)
				records[i].RestoreError = ""
				record := records[i]
				reservation.record = &record
			}
		}
		if reservation.record == nil {
			return ErrBackupNotFound
		}
		checkpoint, err := s.settingRepo.GetValue(txCtx, backupArchiveCheckpointKey)
		if err != nil && !errors.Is(err, ErrSettingNotFound) {
			return err
		}
		reservation.checkpoint = checkpoint
		reservation.records = records
		return s.saveRecordsLocked(context.WithValue(txCtx, backupRecoveryWriteKey{}, true), records)
	})
	return reservation, err
}

type backupRecoveryWriteKey struct{}

// A full database restore can roll backup settings back too. Only the worker
// holding the operation lock may re-register its pre-restore metadata snapshot.
// Ordinary delayed progress callbacks never get this resurrection permission.
func (s *BackupService) finishRestore(reservation *backupRestoreReservation, restoreErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	record := reservation.record
	record.RestoreStatus = "completed"
	if restoreErr != nil {
		record.RestoreStatus = "failed"
		record.RestoreError = truncate(restoreErr.Error(), 2000)
	} else {
		record.RestoredAt = time.Now().UTC().Format(time.RFC3339)
	}
	return s.withBackupRecordLock(ctx, func(txCtx context.Context) error {
		current, err := s.loadRecordsLocked(txCtx)
		if err != nil {
			return err
		}
		byID := make(map[string]BackupRecord, len(current))
		for _, r := range current {
			byID[r.ID] = r
		}
		for i := range reservation.records {
			old := &reservation.records[i]
			if old.ID == record.ID {
				*old = *record
			}
			if latest, ok := byID[old.ID]; ok {
				if latest.RestoreOperationID != old.RestoreOperationID && latest.RestoreStartedAt > record.RestoreStartedAt {
					return ErrRestoreInProgress
				}
				old.MonthlyArchive = mergeBackupArchiveProtection(old.MonthlyArchive, latest.MonthlyArchive)
				if old.MonthlyArchive != nil {
					old.ExpiresAt = ""
				}
			}
		}
		// Preserve monotonic archive date tags even if the restored DB is older.
		raw, err := s.settingRepo.GetValue(txCtx, backupArchiveCheckpointKey)
		if err != nil && !errors.Is(err, ErrSettingNotFound) {
			return err
		}
		var before, after string
		if reservation.checkpoint != "" {
			if err := json.Unmarshal([]byte(reservation.checkpoint), &before); err != nil {
				return err
			}
		}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &after); err != nil {
				return err
			}
		}
		if before > after {
			if err := s.settingRepo.Set(txCtx, backupArchiveCheckpointKey, reservation.checkpoint); err != nil {
				return err
			}
		}
		return s.saveRecordsLocked(context.WithValue(txCtx, backupRecoveryWriteKey{}, true), reservation.records)
	})
}

func mergeBackupArchiveProtection(a, b *BackupMonthlyArchive) *BackupMonthlyArchive {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	merged := &BackupMonthlyArchive{RetainCount: a.RetainCount, Dates: append([]string(nil), a.Dates...)}
	if b.RetainCount == 0 || (a.RetainCount > 0 && b.RetainCount > a.RetainCount) {
		merged.RetainCount = b.RetainCount
	}
	for _, date := range b.Dates {
		found := false
		for _, prior := range merged.Dates {
			if date == prior {
				found = true
				break
			}
		}
		if !found {
			merged.Dates = append(merged.Dates, date)
		}
	}
	return merged
}

// Every metadata write and cleanup shares the same absolute lease deadline,
// including cancellation and stale-recovery paths using Background contexts.
func (s *BackupService) boundBackupFinalization(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(2 * time.Minute)
	if value := s.operationDeadline.Load(); value > 0 && time.Unix(0, value).Before(deadline) {
		deadline = time.Unix(0, value)
	}
	return context.WithDeadline(ctx, deadline)
}
