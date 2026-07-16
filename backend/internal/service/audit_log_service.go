package service

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	auditLogQueueCapacity   = 4096
	auditLogBatchSize       = 100
	auditLogFlushInterval   = time.Second
	auditRetentionCheck     = 24 * time.Hour
	auditRetentionStartup   = 5 * time.Minute
	auditRetentionBatchSize = 5000
)

type AuditLogService struct {
	repo           AuditLogRepository
	settingService *SettingService
	queue          chan *AuditLog
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

func NewAuditLogService(repo AuditLogRepository, settingService *SettingService) *AuditLogService {
	ctx, cancel := context.WithCancel(context.Background())
	return &AuditLogService{repo: repo, settingService: settingService, queue: make(chan *AuditLog, auditLogQueueCapacity), ctx: ctx, cancel: cancel}
}

func ProvideAuditLogService(repo AuditLogRepository, settingService *SettingService) *AuditLogService {
	s := NewAuditLogService(repo, settingService)
	s.Start()
	return s
}

func (s *AuditLogService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.wg.Add(2)
	go s.runWriter()
	go s.runRetention()
}

func (s *AuditLogService) Stop() {
	if s == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

func (s *AuditLogService) Record(entry *AuditLog) {
	if s == nil || entry == nil {
		return
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	select {
	case <-s.ctx.Done():
	case s.queue <- entry:
	default:
		log.Printf("[AuditLog] queue full, dropping action=%s", entry.Action)
	}
}

func (s *AuditLogService) List(ctx context.Context, filter *AuditLogFilter) (*AuditLogList, error) {
	return s.repo.List(ctx, filter)
}
func (s *AuditLogService) GetByID(ctx context.Context, id int64) (*AuditLog, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *AuditLogService) ClearAll(ctx context.Context, trace *AuditLog) (int64, error) {
	count, err := s.repo.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("count audit logs: %w", err)
	}
	if err := s.repo.TruncateAll(ctx); err != nil {
		return 0, fmt.Errorf("truncate audit logs: %w", err)
	}
	if trace != nil {
		trace.Action = AuditActionAuditLogClear
		trace.CreatedAt = time.Now().UTC()
		trace.Extra = map[string]any{"deleted_rows": count}
		if err := s.repo.Insert(ctx, trace); err != nil {
			return count, fmt.Errorf("persist audit clear record: %w", err)
		}
	}
	return count, nil
}

func (s *AuditLogService) runWriter() {
	defer s.wg.Done()
	ticker := time.NewTicker(auditLogFlushInterval)
	defer ticker.Stop()
	batch := make([]*AuditLog, 0, auditLogBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := s.repo.BatchInsert(ctx, batch)
		cancel()
		if err != nil {
			log.Printf("[AuditLog] batch insert failed: %v", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case item := <-s.queue:
			if item != nil {
				batch = append(batch, item)
			}
			if len(batch) >= auditLogBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.ctx.Done():
			for {
				select {
				case item := <-s.queue:
					if item != nil {
						batch = append(batch, item)
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *AuditLogService) runRetention() {
	defer s.wg.Done()
	startupTimer := time.NewTimer(auditRetentionStartup)
	defer startupTimer.Stop()
	select {
	case <-s.ctx.Done():
		return
	case <-startupTimer.C:
	}

	s.runRetentionOnce()
	ticker := time.NewTicker(auditRetentionCheck)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.runRetentionOnce()
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *AuditLogService) runRetentionOnce() {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Minute)
	defer cancel()
	days := 180
	if s.settingService != nil {
		days = s.settingService.GetAuditLogRetentionDays(ctx)
	}
	if days <= 0 {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	for {
		deleted, err := s.repo.DeleteBefore(ctx, cutoff, auditRetentionBatchSize)
		if err != nil {
			log.Printf("[AuditLog] retention cleanup failed: %v", err)
			return
		}
		if deleted == 0 {
			return
		}
	}
}
