package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// This read belongs exclusively to the background control plane.
func (s *DashboardAggregationService) requestRetention(ctx context.Context) (int, bool, error) {
	if s.settingRepo == nil {
		return s.cfg.Retention.UsageLogsDays, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyOpsRuntimeLogConfig)
	if errors.Is(err, ErrSettingNotFound) || (err == nil && raw == "") {
		return s.cfg.Retention.UsageLogsDays, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var cfg OpsRuntimeLogConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return 0, false, err
	}
	if cfg.RequestRetentionOverrideEnabled == nil || !*cfg.RequestRetentionOverrideEnabled {
		return s.cfg.Retention.UsageLogsDays, false, nil
	}
	if cfg.RequestRetentionDays == nil || *cfg.RequestRetentionDays < 0 || *cfg.RequestRetentionDays > 3650 {
		return 0, false, errors.New("invalid request retention")
	}
	return *cfg.RequestRetentionDays, true, nil
}

func (s *DashboardAggregationService) acquireCleanupLock(ctx context.Context, ttl time.Duration) (func(), bool) {
	// Embedded/local deployments without a shared lock backend still need the
	// existing in-process cleanup behavior; production instances use the fixed
	// configured backend below.
	if s.lockCache == nil && s.db == nil {
		return func() {}, true
	} // isolated tests
	return tryAcquireFixedLeaderLock(ctx, s.lockCache, s.db, "usage-retention:cleanup", uuid.NewString(), ttl)
}

// Reuses the existing background settings refresh. Only an explicit override
// schedules cleanup when aggregation is disabled; no gateway reads are added.
func (s *DashboardAggregationService) RefreshRetentionSchedule(ctx context.Context) {
	if s == nil || s.cfg.Enabled || s.timingWheel == nil || s.repo == nil {
		return
	}
	days, override, err := s.requestRetention(ctx)
	s.retentionScheduleMu.Lock()
	defer s.retentionScheduleMu.Unlock()
	enabled := !s.retentionStopped && err == nil && override && days > 0
	if enabled == s.retentionScheduled {
		return
	}
	s.retentionScheduled = enabled
	if !enabled {
		s.timingWheel.Cancel("dashboard:request-retention")
		if s.retentionCancel != nil {
			s.retentionCancel()
			s.retentionCancel = nil
		}
		return
	}
	lifecycle, cancelLifecycle := context.WithCancel(ctx)
	s.retentionCancel = cancelLifecycle
	s.timingWheel.ScheduleRecurring("dashboard:request-retention", dashboardAggregationRetentionInterval, func() {
		ctx, cancel := context.WithTimeout(lifecycle, defaultDashboardAggregationTimeout)
		defer cancel()
		days, override, err := s.requestRetention(ctx)
		if err != nil {
			slog.Warn("request_retention_settings_failed", "error", err)
			return
		}
		if !override || days <= 0 {
			return
		}
		release, ok := s.acquireCleanupLock(ctx, 3*time.Minute)
		if !ok {
			return
		}
		defer release()
		if err := s.repo.CleanupUsageLogs(ctx, time.Now().AddDate(0, 0, -days)); err != nil {
			slog.Warn("request_retention_cleanup_failed", "error", err)
		}
	})
}

type RequestRetentionPreview struct {
	Cutoff        string `json:"cutoff,omitempty"`
	EstimatedRows *int64 `json:"estimated_rows"`
	Permanent     bool   `json:"permanent"`
}

func (s *OpsService) PreviewRequestRetention(ctx context.Context, days int) (*RequestRetentionPreview, error) {
	if days < 0 || days > 3650 {
		return nil, errors.New("request retention must be between 0 and 3650")
	}
	if days == 0 {
		return &RequestRetentionPreview{Permanent: true}, nil
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	result := &RequestRetentionPreview{Cutoff: cutoff.UTC().Format(time.RFC3339)}
	repo, ok := s.opsRepo.(interface {
		EstimateRequestRetention(context.Context, time.Time) (int64, error)
	})
	if ok {
		if rows, err := repo.EstimateRequestRetention(ctx, cutoff); err == nil {
			result.EstimatedRows = &rows
		}
	}
	return result, nil
}

// Stop only the optional retention runner; the timing wheel owns aggregation shutdown.
func (s *DashboardAggregationService) StopRetentionSchedule() {
	if s == nil {
		return
	}
	s.retentionScheduleMu.Lock()
	defer s.retentionScheduleMu.Unlock()
	s.retentionStopped = true
	s.retentionScheduled = false
	if s.retentionCancel != nil {
		s.retentionCancel()
		s.retentionCancel = nil
	}
	if s.timingWheel != nil {
		s.timingWheel.Cancel("dashboard:request-retention")
	}
}
