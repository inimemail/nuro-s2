package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type BackupMonthlyArchiveConfig struct {
	Enabled         bool  `json:"enabled"`
	Days            []int `json:"days"`
	IncludeMonthEnd bool  `json:"include_month_end"`
	RetainCount     int   `json:"retain_count"` // 0 = permanent
}

type BackupMonthlyArchive struct {
	Dates       []string `json:"dates"`
	RetainCount int      `json:"retain_count"`
}

const backupArchiveCheckpointKey = "backup_monthly_archive_checkpoint"
const maxBackupRecordsJSONBytes = 8 << 20

func normalizeBackupMonthlyArchive(cfg *BackupMonthlyArchiveConfig) error {
	if cfg == nil {
		return nil
	}
	if cfg.RetainCount < 0 || cfg.RetainCount > 10000 {
		return fmt.Errorf("archive retain_count must be between 0 and 10000")
	}
	seen := map[int]bool{}
	for _, day := range cfg.Days {
		if day < 1 || day > 31 {
			return fmt.Errorf("archive days must be between 1 and 31")
		}
		seen[day] = true
	}
	cfg.Days = make([]int, 0, len(seen))
	for day := range seen {
		cfg.Days = append(cfg.Days, day)
	}
	sort.Ints(cfg.Days)
	if cfg.Enabled && len(cfg.Days) == 0 && !cfg.IncludeMonthEnd {
		return fmt.Errorf("select an archive day or month end")
	}
	return nil
}

func backupArchiveDates(now time.Time, cfg *BackupMonthlyArchiveConfig, checkpoint string) []string {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	last := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, now.Location()).Day()
	days := append([]int(nil), cfg.Days...)
	if cfg.IncludeMonthEnd {
		days = append(days, last)
	}
	dates := map[string]bool{}
	for _, day := range days {
		if day > last {
			day = last
		}
		if day > now.Day() || day < 1 {
			continue
		}
		date := fmt.Sprintf("%04d-%02d-%02d", now.Year(), now.Month(), day)
		if date > checkpoint {
			dates[date] = true
		}
	}
	out := make([]string, 0, len(dates))
	for date := range dates {
		out = append(out, date)
	}
	sort.Strings(out)
	return out
}

func backupScheduleLocation(expr string) *time.Location {
	fields := strings.Fields(expr)
	if len(fields) > 0 && (strings.HasPrefix(fields[0], "CRON_TZ=") || strings.HasPrefix(fields[0], "TZ=")) {
		if loc, err := time.LoadLocation(strings.SplitN(fields[0], "=", 2)[1]); err == nil {
			return loc
		}
	}
	return time.Local
}

type backupRecordLocker interface {
	WithBackupRecordLock(context.Context, func(context.Context) error) error
}

func (s *BackupService) withBackupRecordLock(ctx context.Context, fn func(context.Context) error) error {
	ctx, cancel := s.boundBackupFinalization(ctx)
	defer cancel()
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()
	if repo, ok := s.settingRepo.(backupRecordLocker); ok {
		return repo.WithBackupRecordLock(ctx, fn)
	}
	// In-memory repositories used by embedded tests have no shared database.
	if s.db != nil || s.lockCache != nil {
		return fmt.Errorf("backup repository must support transactional record locking")
	}
	return fn(ctx)
}

// Called inside the record transaction, so completed record and checkpoint commit together.
func (s *BackupService) assignMonthlyArchive(ctx context.Context, record *BackupRecord) error {
	if record.Status != "completed" || record.TriggeredBy != "scheduled" || record.MonthlyArchive != nil {
		return nil
	}
	schedule, err := s.GetSchedule(ctx)
	if err != nil {
		return err
	}
	if schedule.MonthlyArchive == nil || !schedule.MonthlyArchive.Enabled {
		return nil
	}
	raw, err := s.settingRepo.GetValue(ctx, backupArchiveCheckpointKey)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return err
	}
	var checkpoint string
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
			return fmt.Errorf("invalid archive checkpoint: %w", err)
		}
	}
	finished, err := time.Parse(time.RFC3339, record.FinishedAt)
	if err != nil {
		return err
	}
	dates := backupArchiveDates(finished.In(backupScheduleLocation(schedule.CronExpr)), schedule.MonthlyArchive, checkpoint)
	if len(dates) == 0 {
		return nil
	}
	record.MonthlyArchive = &BackupMonthlyArchive{Dates: dates, RetainCount: schedule.MonthlyArchive.RetainCount}
	record.ExpiresAt = ""
	encoded, _ := json.Marshal(dates[len(dates)-1])
	return s.settingRepo.Set(ctx, backupArchiveCheckpointKey, string(encoded))
}
