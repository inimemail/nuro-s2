package service

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	settingKeyBackupS3Config = "backup_s3_config"
	settingKeyBackupSchedule = "backup_schedule"
	settingKeyBackupRecords  = "backup_records"

	maxBackupRecords                 = 100
	backupObjectCleanupTimeout       = 2 * time.Minute
	defaultBackupPartSizeBytes int64 = 4 * 1024 * 1024 * 1024
)

var (
	ErrBackupS3NotConfigured            = infraerrors.BadRequest("BACKUP_S3_NOT_CONFIGURED", "backup S3 storage is not configured")
	ErrBackupNotFound                   = infraerrors.NotFound("BACKUP_NOT_FOUND", "backup record not found")
	ErrBackupInProgress                 = infraerrors.Conflict("BACKUP_IN_PROGRESS", "a backup is already in progress")
	ErrRestoreInProgress                = infraerrors.Conflict("RESTORE_IN_PROGRESS", "a restore is already in progress")
	ErrBackupRecordsCorrupt             = infraerrors.InternalServer("BACKUP_RECORDS_CORRUPT", "backup records data is corrupted")
	ErrBackupS3ConfigCorrupt            = infraerrors.InternalServer("BACKUP_S3_CONFIG_CORRUPT", "backup S3 config data is corrupted")
	ErrSecretEncryptionKeyNotConfigured = infraerrors.BadRequest(
		"SECRET_ENCRYPTION_KEY_NOT_CONFIGURED",
		"cannot store an object-storage secret without a fixed TOTP_ENCRYPTION_KEY",
	)
)

// ─── 接口定义 ───

// DBDumper abstracts database dump/restore operations
type DBDumper interface {
	Dump(ctx context.Context) (io.ReadCloser, error)
	Restore(ctx context.Context, data io.Reader) error
}

// BackupObjectStore abstracts object storage for backup files
type BackupObjectStore interface {
	Upload(ctx context.Context, key string, body io.Reader, contentType string) (sizeBytes int64, err error)
	Download(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error)
	HeadBucket(ctx context.Context) error
}

// BackupFileObjectStore is optional so existing object-store implementations
// and old backups remain compatible. It is used for bounded-memory part uploads.
type BackupFileObjectStore interface {
	UploadFile(ctx context.Context, key, filePath, contentType string) (int64, error)
}

// BackupObjectStoreFactory creates an object store from S3 config
type BackupObjectStoreFactory func(ctx context.Context, cfg *BackupS3Config) (BackupObjectStore, error)

// ─── 数据模型 ───

// BackupS3Config S3 兼容存储配置（支持 Cloudflare R2）
type BackupS3Config struct {
	Endpoint                string `json:"endpoint"` // e.g. https://<account_id>.r2.cloudflarestorage.com
	Region                  string `json:"region"`   // R2 用 "auto"
	Bucket                  string `json:"bucket"`
	AccessKeyID             string `json:"access_key_id"`
	SecretAccessKey         string `json:"secret_access_key,omitempty"` //nolint:revive // field name follows AWS convention
	SecretEncryptionVersion int    `json:"secret_encryption_version,omitempty"`
	Prefix                  string `json:"prefix"` // S3 key 前缀，如 "backups/"
	ForcePathStyle          bool   `json:"force_path_style"`
}

// IsConfigured 检查必要字段是否已配置
func (c *BackupS3Config) IsConfigured() bool {
	return c.Bucket != "" && c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// BackupScheduleConfig 定时备份配置
type BackupScheduleConfig struct {
	Enabled     bool   `json:"enabled"`
	CronExpr    string `json:"cron_expr"`    // cron 表达式，如 "0 2 * * *" 每天凌晨2点
	RetainDays  int    `json:"retain_days"`  // 备份文件过期天数，默认14，0=不自动清理
	RetainCount int    `json:"retain_count"` // 最多保留份数，0=不限制
}

// BackupRecord 备份记录
type BackupRecord struct {
	ID            string       `json:"id"`
	Status        string       `json:"status"`      // pending, running, completed, failed
	BackupType    string       `json:"backup_type"` // postgres
	FileName      string       `json:"file_name"`
	S3Key         string       `json:"s3_key"`
	Parts         []BackupPart `json:"parts,omitempty"`
	SizeBytes     int64        `json:"size_bytes"`
	TriggeredBy   string       `json:"triggered_by"` // manual, scheduled
	ErrorMsg      string       `json:"error_message,omitempty"`
	StartedAt     string       `json:"started_at"`
	FinishedAt    string       `json:"finished_at,omitempty"`
	ExpiresAt     string       `json:"expires_at,omitempty"`     // 过期时间
	Progress      string       `json:"progress,omitempty"`       // "dumping", "uploading", ""
	RestoreStatus string       `json:"restore_status,omitempty"` // "", "running", "completed", "failed"
	RestoreError  string       `json:"restore_error,omitempty"`
	RestoredAt    string       `json:"restored_at,omitempty"`
}

type BackupPart struct {
	Index     int    `json:"index"`
	S3Key     string `json:"s3_key"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256,omitempty"`
}

type BackupDownloadPart struct {
	Index     int    `json:"index"`
	SizeBytes int64  `json:"size_bytes"`
	URL       string `json:"url"`
}

type BackupDownloadResponse struct {
	URL   string               `json:"url,omitempty"`
	Parts []BackupDownloadPart `json:"parts,omitempty"`
}

// BackupService 数据库备份恢复服务
type BackupService struct {
	settingRepo             SettingRepository
	dbCfg                   *config.DatabaseConfig
	encryptor               SecretEncryptor
	storeFactory            BackupObjectStoreFactory
	dumper                  DBDumper
	encryptionKeyConfigured bool

	opMu      sync.Mutex // 保护 backingUp/restoring 标志
	backingUp bool
	restoring bool

	storeMu             sync.Mutex // 保护 store/s3Cfg 缓存
	store               BackupObjectStore
	s3Cfg               *BackupS3Config
	s3ConfigInvalidator func()

	recordsMu sync.Mutex // 保护 records 的 load/save 操作

	cronMu      sync.Mutex
	cronSched   *cron.Cron
	cronEntryID cron.EntryID

	wg            sync.WaitGroup     // 追踪活跃的备份/恢复 goroutine
	shuttingDown  atomic.Bool        // 阻止新备份启动
	bgCtx         context.Context    // 所有后台操作的 parent context
	bgCancel      context.CancelFunc // 取消所有活跃后台操作
	partSizeBytes int64
}

func NewBackupService(
	settingRepo SettingRepository,
	cfg *config.Config,
	encryptor SecretEncryptor,
	storeFactory BackupObjectStoreFactory,
	dumper DBDumper,
) *BackupService {
	bgCtx, bgCancel := context.WithCancel(context.Background())
	configured := cfg != nil && cfg.Totp.EncryptionKeyConfigured
	dbCfg := &config.DatabaseConfig{}
	if cfg != nil {
		dbCfg = &cfg.Database
	}
	return &BackupService{
		settingRepo:             settingRepo,
		dbCfg:                   dbCfg,
		encryptor:               encryptor,
		storeFactory:            storeFactory,
		dumper:                  dumper,
		encryptionKeyConfigured: configured,
		bgCtx:                   bgCtx,
		bgCancel:                bgCancel,
		partSizeBytes:           defaultBackupPartSizeBytes,
	}
}

func (s *BackupService) EncryptionKeyConfigured() bool { return s != nil && s.encryptionKeyConfigured }

func (s *BackupService) SetS3ConfigInvalidator(invalidate func()) {
	if s == nil {
		return
	}
	s.storeMu.Lock()
	s.s3ConfigInvalidator = invalidate
	s.storeMu.Unlock()
}

// LoadS3ConfigForImage exposes the already-decrypted backup credentials only
// inside the service layer, so image settings can reuse them without persisting
// a second copy of the secret.
func (s *BackupService) LoadS3ConfigForImage(ctx context.Context) (*BackupS3Config, error) {
	if s == nil {
		return nil, ErrBackupS3NotConfigured
	}
	return s.loadS3Config(ctx)
}

// Start 启动定时备份调度器并清理孤立记录
func (s *BackupService) Start() {
	s.cronSched = cron.New()
	s.cronSched.Start()

	// 清理重启后孤立的 running 记录
	s.recoverStaleRecords()

	// 加载已有的定时配置
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	schedule, err := s.GetSchedule(ctx)
	if err != nil {
		logger.LegacyPrintf("service.backup", "[Backup] 加载定时备份配置失败: %v", err)
		return
	}
	if schedule.Enabled && schedule.CronExpr != "" {
		if err := s.applyCronSchedule(schedule); err != nil {
			logger.LegacyPrintf("service.backup", "[Backup] 应用定时备份配置失败: %v", err)
		}
	}
}

// recoverStaleRecords 启动时将孤立的 running 记录标记为 failed
func (s *BackupService) recoverStaleRecords() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	records, err := s.loadRecords(ctx)
	if err != nil {
		return
	}
	for i := range records {
		if records[i].Status == "running" {
			stale := records[i]
			records[i].Status = "failed"
			records[i].ErrorMsg = "interrupted by server restart"
			records[i].Progress = ""
			records[i].FinishedAt = time.Now().Format(time.RFC3339)
			_ = s.saveRecord(ctx, &records[i])
			if len(backupObjectKeys(&stale)) > 0 {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), backupObjectCleanupTimeout)
				if cleanupErr := s.deleteBackupObjects(cleanupCtx, &stale); cleanupErr != nil {
					records[i].ErrorMsg += "; cleanup failed: " + cleanupErr.Error()
					_ = s.saveRecord(context.Background(), &records[i])
				}
				cleanupCancel()
			}
			logger.LegacyPrintf("service.backup", "[Backup] recovered stale running record: %s", records[i].ID)
		}
		if records[i].RestoreStatus == "running" {
			records[i].RestoreStatus = "failed"
			records[i].RestoreError = "interrupted by server restart"
			_ = s.saveRecord(ctx, &records[i])
			logger.LegacyPrintf("service.backup", "[Backup] recovered stale restoring record: %s", records[i].ID)
		}
	}
}

const backupFastShutdownTimeout = 5 * time.Second

// Stop 停止定时备份并等待活跃操作完成。
func (s *BackupService) Stop() {
	s.stop(false, 5*time.Minute)
}

// StopFast is used during process termination. New work is rejected, active
// backup/restore contexts are cancelled immediately, and infrastructure
// teardown waits only for the bounded cancellation window.
func (s *BackupService) StopFast() {
	s.stop(true, backupFastShutdownTimeout)
}

func (s *BackupService) stop(cancelImmediately bool, timeout time.Duration) {
	if s == nil {
		return
	}
	s.shuttingDown.Store(true)

	s.cronMu.Lock()
	if s.cronSched != nil {
		s.cronSched.Stop()
	}
	s.cronMu.Unlock()
	if cancelImmediately && s.bgCancel != nil {
		s.bgCancel()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		logger.LegacyPrintf("service.backup", "[Backup] all active operations finished")
	case <-timer.C:
		logger.LegacyPrintf("service.backup", "[Backup] shutdown timeout after %s, cancelling active operations", timeout)
		if s.bgCancel != nil {
			s.bgCancel()
		}
	}
}

// ─── S3 配置管理 ───

func (s *BackupService) GetS3Config(ctx context.Context) (*BackupS3Config, error) {
	cfg, err := s.loadS3Config(ctx)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return &BackupS3Config{}, nil
	}
	// 脱敏返回
	cfg.SecretAccessKey = ""
	cfg.SecretEncryptionVersion = 0
	return cfg, nil
}

func (s *BackupService) UpdateS3Config(ctx context.Context, cfg BackupS3Config) (*BackupS3Config, error) {
	cfg.SecretEncryptionVersion = 0
	// 如果没提供 secret，保留原有值
	if cfg.SecretAccessKey == "" {
		old, err := s.loadStoredS3Config(ctx)
		if err != nil {
			return nil, err
		}
		if old != nil {
			cfg.SecretAccessKey = old.SecretAccessKey
			cfg.SecretEncryptionVersion = old.SecretEncryptionVersion
		}
	} else {
		if !s.encryptionKeyConfigured {
			return nil, ErrSecretEncryptionKeyNotConfigured
		}
		// 加密 SecretAccessKey
		encrypted, err := encryptStoredSecret(s.encryptor, cfg.SecretAccessKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt secret: %w", err)
		}
		cfg.SecretAccessKey = encrypted
		cfg.SecretEncryptionVersion = storedSecretEncryptionVersion
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal s3 config: %w", err)
	}
	if err := s.settingRepo.Set(ctx, settingKeyBackupS3Config, string(data)); err != nil {
		return nil, fmt.Errorf("save s3 config: %w", err)
	}

	// 清除缓存的 S3 客户端
	s.storeMu.Lock()
	s.store = nil
	s.s3Cfg = nil
	invalidate := s.s3ConfigInvalidator
	s.storeMu.Unlock()
	if invalidate != nil {
		invalidate()
	}

	cfg.SecretAccessKey = ""
	cfg.SecretEncryptionVersion = 0
	return &cfg, nil
}

func (s *BackupService) TestS3Connection(ctx context.Context, cfg BackupS3Config) error {
	// 如果没提供 secret，用已保存的
	if cfg.SecretAccessKey == "" {
		old, _ := s.loadS3Config(ctx)
		if old != nil {
			cfg.SecretAccessKey = old.SecretAccessKey
		}
	}

	if cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return fmt.Errorf("incomplete S3 config: bucket, access_key_id, secret_access_key are required")
	}

	store, err := s.storeFactory(ctx, &cfg)
	if err != nil {
		return err
	}
	return store.HeadBucket(ctx)
}

// ─── 定时备份管理 ───

func (s *BackupService) GetSchedule(ctx context.Context) (*BackupScheduleConfig, error) {
	raw, err := s.settingRepo.GetValue(ctx, settingKeyBackupSchedule)
	if err != nil || raw == "" {
		return &BackupScheduleConfig{}, nil
	}
	var cfg BackupScheduleConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return &BackupScheduleConfig{}, nil
	}
	return &cfg, nil
}

func (s *BackupService) UpdateSchedule(ctx context.Context, cfg BackupScheduleConfig) (*BackupScheduleConfig, error) {
	if cfg.Enabled && cfg.CronExpr == "" {
		return nil, infraerrors.BadRequest("INVALID_CRON", "cron expression is required when schedule is enabled")
	}
	// 验证 cron 表达式
	if cfg.CronExpr != "" {
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		if _, err := parser.Parse(cfg.CronExpr); err != nil {
			return nil, infraerrors.BadRequest("INVALID_CRON", fmt.Sprintf("invalid cron expression: %v", err))
		}
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal schedule config: %w", err)
	}
	if err := s.settingRepo.Set(ctx, settingKeyBackupSchedule, string(data)); err != nil {
		return nil, fmt.Errorf("save schedule config: %w", err)
	}

	// 应用或停止定时任务
	if cfg.Enabled {
		if err := s.applyCronSchedule(&cfg); err != nil {
			return nil, err
		}
	} else {
		s.removeCronSchedule()
	}

	return &cfg, nil
}

func (s *BackupService) applyCronSchedule(cfg *BackupScheduleConfig) error {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()

	if s.cronSched == nil {
		return fmt.Errorf("cron scheduler not initialized")
	}

	// 移除旧任务
	if s.cronEntryID != 0 {
		s.cronSched.Remove(s.cronEntryID)
		s.cronEntryID = 0
	}

	entryID, err := s.cronSched.AddFunc(cfg.CronExpr, func() {
		s.runScheduledBackup()
	})
	if err != nil {
		return infraerrors.BadRequest("INVALID_CRON", fmt.Sprintf("failed to schedule: %v", err))
	}
	s.cronEntryID = entryID
	logger.LegacyPrintf("service.backup", "[Backup] 定时备份已启用: %s", cfg.CronExpr)
	return nil
}

func (s *BackupService) removeCronSchedule() {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()
	if s.cronSched != nil && s.cronEntryID != 0 {
		s.cronSched.Remove(s.cronEntryID)
		s.cronEntryID = 0
		logger.LegacyPrintf("service.backup", "[Backup] 定时备份已停用")
	}
}

func (s *BackupService) runScheduledBackup() {
	s.wg.Add(1)
	defer s.wg.Done()

	ctx, cancel := context.WithTimeout(s.bgCtx, 30*time.Minute)
	defer cancel()

	// 读取定时备份配置中的过期天数
	schedule, _ := s.GetSchedule(ctx)
	expireDays := 14 // 默认14天过期
	if schedule != nil && schedule.RetainDays > 0 {
		expireDays = schedule.RetainDays
	}

	logger.LegacyPrintf("service.backup", "[Backup] 开始执行定时备份, 过期天数: %d", expireDays)
	record, err := s.CreateBackup(ctx, "scheduled", expireDays)
	if err != nil {
		if errors.Is(err, ErrBackupInProgress) {
			logger.LegacyPrintf("service.backup", "[Backup] 定时备份跳过: 已有备份正在进行中")
		} else {
			logger.LegacyPrintf("service.backup", "[Backup] 定时备份失败: %v", err)
		}
		return
	}
	logger.LegacyPrintf("service.backup", "[Backup] 定时备份完成: id=%s size=%d", record.ID, record.SizeBytes)

	// 清理过期备份（复用已加载的 schedule）
	if schedule == nil {
		return
	}
	if err := s.cleanupOldBackups(ctx, schedule); err != nil {
		logger.LegacyPrintf("service.backup", "[Backup] 清理过期备份失败: %v", err)
	}
}

// ─── 备份/恢复核心 ───

// CreateBackup 创建全量数据库备份并上传到 S3（流式处理）
// expireDays: 备份过期天数，0=永不过期，默认14天
func (s *BackupService) CreateBackup(ctx context.Context, triggeredBy string, expireDays int) (*BackupRecord, error) {
	if s.shuttingDown.Load() {
		return nil, infraerrors.ServiceUnavailable("SERVER_SHUTTING_DOWN", "server is shutting down")
	}

	s.opMu.Lock()
	if s.backingUp {
		s.opMu.Unlock()
		return nil, ErrBackupInProgress
	}
	s.backingUp = true
	s.opMu.Unlock()
	defer func() {
		s.opMu.Lock()
		s.backingUp = false
		s.opMu.Unlock()
	}()

	s3Cfg, err := s.loadS3Config(ctx)
	if err != nil {
		return nil, err
	}
	if s3Cfg == nil || !s3Cfg.IsConfigured() {
		return nil, ErrBackupS3NotConfigured
	}

	objectStore, err := s.getOrCreateStore(ctx, s3Cfg)
	if err != nil {
		return nil, fmt.Errorf("init object store: %w", err)
	}

	now := time.Now()
	backupID := uuid.New().String()[:8]
	fileName := fmt.Sprintf("%s_%s.sql.gz", s.dbCfg.DBName, now.Format("20060102_150405"))
	s3Key := s.buildS3Key(s3Cfg, fileName)

	var expiresAt string
	if expireDays > 0 {
		expiresAt = now.AddDate(0, 0, expireDays).Format(time.RFC3339)
	}

	record := &BackupRecord{
		ID:          backupID,
		Status:      "running",
		BackupType:  "postgres",
		FileName:    fileName,
		S3Key:       s3Key,
		TriggeredBy: triggeredBy,
		StartedAt:   now.Format(time.RFC3339),
		ExpiresAt:   expiresAt,
	}
	if err := s.saveRecord(ctx, record); err != nil {
		return nil, fmt.Errorf("save initial record: %w", err)
	}

	archivePath, sizeBytes, err := s.createCompressedBackupFile(ctx)
	if err != nil {
		record.Status = "failed"
		record.ErrorMsg = err.Error()
		record.FinishedAt = time.Now().Format(time.RFC3339)
		_ = s.saveRecord(ctx, record)
		return record, err
	}
	defer func() { _ = cleanupBackupFiles(archivePath) }()
	record.SizeBytes = sizeBytes
	if err := s.uploadBackupArchive(ctx, record, objectStore, archivePath, func() error {
		return s.saveRecord(ctx, record)
	}); err != nil {
		record.Status = "failed"
		record.ErrorMsg = err.Error()
		record.FinishedAt = time.Now().Format(time.RFC3339)
		_ = s.saveRecord(ctx, record)
		return record, err
	}
	record.Status = "completed"
	record.FinishedAt = time.Now().Format(time.RFC3339)
	if err := s.saveRecord(ctx, record); err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), backupObjectCleanupTimeout)
		cleanupErr := deleteBackupObjectsFromStore(cleanupCtx, objectStore, record)
		cleanupCancel()
		record.Status = "failed"
		record.ErrorMsg = "persist completed backup record: " + err.Error()
		if cleanupErr != nil {
			record.ErrorMsg += "; cleanup failed: " + cleanupErr.Error()
		}
		_ = s.saveRecord(context.Background(), record)
		return record, fmt.Errorf("persist completed backup record: %w", err)
	}

	return record, nil
}

func (s *BackupService) createCompressedBackupFile(ctx context.Context) (string, int64, error) {
	dumpReader, err := s.dumper.Dump(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("pg_dump: %w", err)
	}
	defer dumpReader.Close()
	f, err := os.CreateTemp("", "sub2api-backup-*.sql.gz")
	if err != nil {
		return "", 0, fmt.Errorf("create backup archive: %w", err)
	}
	path := f.Name()
	if err := f.Chmod(0600); err != nil {
		f.Close()
		cleanupBackupFiles(path)
		return "", 0, err
	}
	zw := gzip.NewWriter(f)
	if _, err := io.Copy(zw, dumpReader); err != nil {
		zw.Close()
		f.Close()
		cleanupBackupFiles(path)
		return "", 0, fmt.Errorf("gzip dump: %w", err)
	}
	if err := zw.Close(); err != nil {
		f.Close()
		cleanupBackupFiles(path)
		return "", 0, fmt.Errorf("close gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanupBackupFiles(path)
		return "", 0, fmt.Errorf("close backup archive: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		cleanupBackupFiles(path)
		return "", 0, err
	}
	return path, info.Size(), nil
}

type localBackupPart struct {
	Index     int
	Path      string
	SizeBytes int64
	SHA256    string
}

func splitBackupFile(srcPath string, partSize int64) ([]localBackupPart, error) {
	if partSize <= 0 {
		return nil, fmt.Errorf("backup part size must be positive")
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, errors.New("backup archive is empty")
	}
	var parts []localBackupPart
	for index := 1; ; index++ {
		part, err := os.CreateTemp("", "sub2api-backup-part-*")
		if err != nil {
			cleanupBackupPartFiles(parts)
			return nil, err
		}
		_ = part.Chmod(0600)
		hash := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(part, hash), src, partSize)
		if copyErr != nil && !errors.Is(copyErr, io.EOF) && !errors.Is(copyErr, io.ErrUnexpectedEOF) {
			part.Close()
			cleanupBackupFiles(part.Name())
			cleanupBackupPartFiles(parts)
			return nil, copyErr
		}
		closeErr := part.Close()
		if closeErr != nil {
			cleanupBackupFiles(part.Name())
			cleanupBackupPartFiles(parts)
			return nil, closeErr
		}
		if written == 0 {
			cleanupBackupFiles(part.Name())
			break
		}
		parts = append(parts, localBackupPart{Index: index, Path: part.Name(), SizeBytes: written, SHA256: hex.EncodeToString(hash.Sum(nil))})
		if written < partSize {
			break
		}
	}
	return parts, nil
}

func cleanupBackupPartFiles(parts []localBackupPart) {
	for _, p := range parts {
		_ = cleanupBackupFiles(p.Path)
	}
}
func cleanupBackupFiles(paths ...string) error {
	var errs []error
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func backupObjectKeys(record *BackupRecord) []string {
	if record == nil {
		return nil
	}
	keys := make([]string, 0, len(record.Parts)+1)
	if record.S3Key != "" {
		keys = append(keys, record.S3Key)
	}
	for _, p := range record.Parts {
		if p.S3Key != "" {
			keys = append(keys, p.S3Key)
		}
	}
	return keys
}

func (s *BackupService) uploadBackupArchive(
	ctx context.Context,
	record *BackupRecord,
	store BackupObjectStore,
	archivePath string,
	persist func() error,
) error {
	if store == nil {
		return errors.New("backup object store is nil")
	}
	if info, err := os.Stat(archivePath); err != nil {
		return err
	} else if info.Size() <= s.partSizeBytes {
		if uploader, ok := store.(BackupFileObjectStore); ok {
			if _, err := uploader.UploadFile(ctx, record.S3Key, archivePath, "application/gzip"); err != nil {
				return fmt.Errorf("backup upload: %w", err)
			}
			return nil
		}
		f, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = store.Upload(ctx, record.S3Key, f, "application/gzip")
		if err != nil {
			return fmt.Errorf("backup upload: %w", err)
		}
		return nil
	}
	parts, err := splitBackupFile(archivePath, s.partSizeBytes)
	if err != nil {
		return err
	}
	defer cleanupBackupPartFiles(parts)
	uploader, ok := store.(BackupFileObjectStore)
	if !ok {
		return fmt.Errorf("object store does not support file upload for multipart backup")
	}
	initialPartCount := len(record.Parts)
	cleanupUploaded := func(extraKey string) {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), backupObjectCleanupTimeout)
		defer cleanupCancel()
		keys := backupObjectKeys(&BackupRecord{Parts: record.Parts[initialPartCount:]})
		if extraKey != "" {
			keys = append(keys, extraKey)
		}
		for _, uploaded := range keys {
			_ = store.Delete(cleanupCtx, uploaded)
		}
		record.Parts = record.Parts[:initialPartCount]
	}
	for _, part := range parts {
		key := fmt.Sprintf("%s.part-%04d", record.S3Key, part.Index)
		if _, err := uploader.UploadFile(ctx, key, part.Path, "application/gzip"); err != nil {
			// Remove every part uploaded in this batch, including the current key
			// when a store reports an ambiguous post-upload failure. Cleanup uses
			// the already-created store so it cannot be blocked by a settings read.
			cleanupUploaded(key)
			return fmt.Errorf("upload backup part %d: %w", part.Index, err)
		}
		record.Parts = append(record.Parts, BackupPart{Index: part.Index, S3Key: key, SizeBytes: part.SizeBytes, SHA256: part.SHA256})
		if persist != nil {
			if err := persist(); err != nil {
				cleanupUploaded("")
				return fmt.Errorf("persist backup part %d: %w", part.Index, err)
			}
		}
	}
	return nil
}

func (s *BackupService) deleteBackupObjects(ctx context.Context, record *BackupRecord) error {
	cfg, err := s.loadS3Config(ctx)
	if err != nil {
		return err
	}
	store, err := s.getOrCreateStore(ctx, cfg)
	if err != nil {
		return err
	}
	return deleteBackupObjectsFromStore(ctx, store, record)
}

func deleteBackupObjectsFromStore(ctx context.Context, store BackupObjectStore, record *BackupRecord) error {
	if store == nil {
		return errors.New("backup object store is nil")
	}
	var errs []error
	for _, key := range backupObjectKeys(record) {
		if err := store.Delete(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// StartBackup 异步创建备份，立即返回 running 状态的记录
func (s *BackupService) StartBackup(ctx context.Context, triggeredBy string, expireDays int) (*BackupRecord, error) {
	if s.shuttingDown.Load() {
		return nil, infraerrors.ServiceUnavailable("SERVER_SHUTTING_DOWN", "server is shutting down")
	}

	s.opMu.Lock()
	if s.backingUp {
		s.opMu.Unlock()
		return nil, ErrBackupInProgress
	}
	s.backingUp = true
	s.opMu.Unlock()

	// 初始化阶段出错时自动重置标志
	launched := false
	defer func() {
		if !launched {
			s.opMu.Lock()
			s.backingUp = false
			s.opMu.Unlock()
		}
	}()

	// 在返回前加载 S3 配置和创建 store，避免 goroutine 中配置被修改
	s3Cfg, err := s.loadS3Config(ctx)
	if err != nil {
		return nil, err
	}
	if s3Cfg == nil || !s3Cfg.IsConfigured() {
		return nil, ErrBackupS3NotConfigured
	}

	objectStore, err := s.getOrCreateStore(ctx, s3Cfg)
	if err != nil {
		return nil, fmt.Errorf("init object store: %w", err)
	}

	now := time.Now()
	backupID := uuid.New().String()[:8]
	fileName := fmt.Sprintf("%s_%s.sql.gz", s.dbCfg.DBName, now.Format("20060102_150405"))
	s3Key := s.buildS3Key(s3Cfg, fileName)

	var expiresAt string
	if expireDays > 0 {
		expiresAt = now.AddDate(0, 0, expireDays).Format(time.RFC3339)
	}

	record := &BackupRecord{
		ID:          backupID,
		Status:      "running",
		BackupType:  "postgres",
		FileName:    fileName,
		S3Key:       s3Key,
		TriggeredBy: triggeredBy,
		StartedAt:   now.Format(time.RFC3339),
		ExpiresAt:   expiresAt,
		Progress:    "pending",
	}

	if err := s.saveRecord(ctx, record); err != nil {
		return nil, fmt.Errorf("save initial record: %w", err)
	}

	launched = true
	// 在启动 goroutine 前完成拷贝，避免数据竞争
	result := *record

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.opMu.Lock()
			s.backingUp = false
			s.opMu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				logger.LegacyPrintf("service.backup", "[Backup] panic recovered: %v", r)
				record.Status = "failed"
				record.ErrorMsg = fmt.Sprintf("internal panic: %v", r)
				record.Progress = ""
				record.FinishedAt = time.Now().Format(time.RFC3339)
				_ = s.saveRecord(context.Background(), record)
			}
		}()
		s.executeBackup(record, objectStore)
	}()

	return &result, nil
}

// executeBackup 后台执行备份（独立于 HTTP context）
func (s *BackupService) executeBackup(record *BackupRecord, objectStore BackupObjectStore) {
	ctx, cancel := context.WithTimeout(s.bgCtx, 30*time.Minute)
	defer cancel()

	// 阶段1: pg_dump
	record.Progress = "dumping"
	_ = s.saveRecord(ctx, record)

	archivePath, sizeBytes, err := s.createCompressedBackupFile(ctx)
	if err != nil {
		record.Status = "failed"
		record.ErrorMsg = fmt.Sprintf("pg_dump failed: %v", err)
		record.Progress = ""
		record.FinishedAt = time.Now().Format(time.RFC3339)
		_ = s.saveRecord(context.Background(), record)
		return
	}
	defer func() { _ = cleanupBackupFiles(archivePath) }()
	record.SizeBytes = sizeBytes

	// 阶段2: 单对象或分卷上传
	record.Progress = "uploading"
	_ = s.saveRecord(ctx, record)
	if err := s.uploadBackupArchive(ctx, record, objectStore, archivePath, func() error {
		return s.saveRecord(ctx, record)
	}); err != nil {
		record.Status = "failed"
		record.ErrorMsg = err.Error()
		record.Progress = ""
		record.FinishedAt = time.Now().Format(time.RFC3339)
		_ = s.saveRecord(context.Background(), record)
		return
	}

	record.Status = "completed"
	record.Progress = ""
	record.FinishedAt = time.Now().Format(time.RFC3339)
	if err := s.saveRecord(context.Background(), record); err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), backupObjectCleanupTimeout)
		cleanupErr := deleteBackupObjectsFromStore(cleanupCtx, objectStore, record)
		cleanupCancel()
		record.Status = "failed"
		record.ErrorMsg = "persist completed backup record: " + err.Error()
		if cleanupErr != nil {
			record.ErrorMsg += "; cleanup failed: " + cleanupErr.Error()
		}
		_ = s.saveRecord(context.Background(), record)
		logger.LegacyPrintf("service.backup", "[Backup] 保存备份记录失败: %v", err)
	}
}

// RestoreBackup 从 S3 下载备份并流式恢复到数据库
func (s *BackupService) RestoreBackup(ctx context.Context, backupID string) error {
	s.opMu.Lock()
	if s.restoring {
		s.opMu.Unlock()
		return ErrRestoreInProgress
	}
	s.restoring = true
	s.opMu.Unlock()
	defer func() {
		s.opMu.Lock()
		s.restoring = false
		s.opMu.Unlock()
	}()

	record, err := s.GetBackupRecord(ctx, backupID)
	if err != nil {
		return err
	}
	if record.Status != "completed" {
		return infraerrors.BadRequest("BACKUP_NOT_COMPLETED", "can only restore from a completed backup")
	}

	s3Cfg, err := s.loadS3Config(ctx)
	if err != nil {
		return err
	}
	objectStore, err := s.getOrCreateStore(ctx, s3Cfg)
	if err != nil {
		return fmt.Errorf("init object store: %w", err)
	}

	body, cleanup, err := s.openVerifiedBackupArchive(ctx, record, objectStore)
	if err != nil {
		return fmt.Errorf("S3 download failed: %w", err)
	}
	defer cleanup()
	defer func() { _ = body.Close() }()

	// 流式解压 gzip -> psql（不将全部数据加载到内存）
	gzReader, err := gzip.NewReader(body)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	// 流式恢复
	if err := s.dumper.Restore(ctx, gzReader); err != nil {
		return fmt.Errorf("pg restore: %w", err)
	}

	return nil
}

// StartRestore 异步恢复备份，立即返回
func (s *BackupService) StartRestore(ctx context.Context, backupID string) (*BackupRecord, error) {
	if s.shuttingDown.Load() {
		return nil, infraerrors.ServiceUnavailable("SERVER_SHUTTING_DOWN", "server is shutting down")
	}

	s.opMu.Lock()
	if s.restoring {
		s.opMu.Unlock()
		return nil, ErrRestoreInProgress
	}
	s.restoring = true
	s.opMu.Unlock()

	// 初始化阶段出错时自动重置标志
	launched := false
	defer func() {
		if !launched {
			s.opMu.Lock()
			s.restoring = false
			s.opMu.Unlock()
		}
	}()

	record, err := s.GetBackupRecord(ctx, backupID)
	if err != nil {
		return nil, err
	}
	if record.Status != "completed" {
		return nil, infraerrors.BadRequest("BACKUP_NOT_COMPLETED", "can only restore from a completed backup")
	}

	s3Cfg, err := s.loadS3Config(ctx)
	if err != nil {
		return nil, err
	}
	objectStore, err := s.getOrCreateStore(ctx, s3Cfg)
	if err != nil {
		return nil, fmt.Errorf("init object store: %w", err)
	}

	record.RestoreStatus = "running"
	_ = s.saveRecord(ctx, record)

	launched = true
	result := *record

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.opMu.Lock()
			s.restoring = false
			s.opMu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				logger.LegacyPrintf("service.backup", "[Backup] restore panic recovered: %v", r)
				record.RestoreStatus = "failed"
				record.RestoreError = fmt.Sprintf("internal panic: %v", r)
				_ = s.saveRecord(context.Background(), record)
			}
		}()
		s.executeRestore(record, objectStore)
	}()

	return &result, nil
}

// executeRestore 后台执行恢复
func (s *BackupService) executeRestore(record *BackupRecord, objectStore BackupObjectStore) {
	ctx, cancel := context.WithTimeout(s.bgCtx, 30*time.Minute)
	defer cancel()

	body, cleanup, err := s.openVerifiedBackupArchive(ctx, record, objectStore)
	if err != nil {
		record.RestoreStatus = "failed"
		record.RestoreError = fmt.Sprintf("S3 download failed: %v", err)
		_ = s.saveRecord(context.Background(), record)
		return
	}
	defer cleanup()
	defer func() { _ = body.Close() }()

	gzReader, err := gzip.NewReader(body)
	if err != nil {
		record.RestoreStatus = "failed"
		record.RestoreError = fmt.Sprintf("gzip reader: %v", err)
		_ = s.saveRecord(context.Background(), record)
		return
	}
	defer func() { _ = gzReader.Close() }()

	if err := s.dumper.Restore(ctx, gzReader); err != nil {
		record.RestoreStatus = "failed"
		record.RestoreError = fmt.Sprintf("pg restore: %v", err)
		_ = s.saveRecord(context.Background(), record)
		return
	}

	record.RestoreStatus = "completed"
	record.RestoredAt = time.Now().Format(time.RFC3339)
	if err := s.saveRecord(context.Background(), record); err != nil {
		logger.LegacyPrintf("service.backup", "[Backup] 保存恢复记录失败: %v", err)
	}
}

func (s *BackupService) openVerifiedBackupArchive(ctx context.Context, record *BackupRecord, store BackupObjectStore) (io.ReadCloser, func(), error) {
	if len(record.Parts) == 0 {
		body, err := store.Download(ctx, record.S3Key)
		return body, func() {}, err
	}
	path, err := os.CreateTemp("", "sub2api-restore-*.sql.gz")
	if err != nil {
		return nil, func() {}, err
	}
	archivePath := path.Name()
	_ = path.Chmod(0600)
	cleanup := func() { _ = path.Close(); _ = cleanupBackupFiles(archivePath) }
	for _, part := range record.Parts {
		body, err := store.Download(ctx, part.S3Key)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(path, hash), body)
		_ = body.Close()
		if copyErr != nil || written != part.SizeBytes || (part.SHA256 != "" && !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), part.SHA256)) {
			cleanup()
			if copyErr != nil {
				return nil, func() {}, copyErr
			}
			return nil, func() {}, fmt.Errorf("backup part %d integrity check failed", part.Index)
		}
	}
	if _, err := path.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return path, cleanup, nil
}

// ─── 备份记录管理 ───

func (s *BackupService) ListBackups(ctx context.Context) ([]BackupRecord, error) {
	records, err := s.loadRecords(ctx)
	if err != nil {
		return nil, err
	}
	// 倒序返回（最新在前）
	sort.Slice(records, func(i, j int) bool {
		return records[i].StartedAt > records[j].StartedAt
	})
	return records, nil
}

func (s *BackupService) GetBackupRecord(ctx context.Context, backupID string) (*BackupRecord, error) {
	records, err := s.loadRecords(ctx)
	if err != nil {
		return nil, err
	}
	for i := range records {
		if records[i].ID == backupID {
			return &records[i], nil
		}
	}
	return nil, ErrBackupNotFound
}

func (s *BackupService) DeleteBackup(ctx context.Context, backupID string) error {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()

	records, err := s.loadRecordsLocked(ctx)
	if err != nil {
		return err
	}

	var found *BackupRecord
	var remaining []BackupRecord
	for i := range records {
		if records[i].ID == backupID {
			found = &records[i]
		} else {
			remaining = append(remaining, records[i])
		}
	}
	if found == nil {
		return ErrBackupNotFound
	}

	// 从 S3 删除
	if len(backupObjectKeys(found)) > 0 && (found.Status == "completed" || found.Status == "failed" || found.Status == "running") {
		s3Cfg, err := s.loadS3Config(ctx)
		if err == nil && s3Cfg != nil && s3Cfg.IsConfigured() {
			objectStore, err := s.getOrCreateStore(ctx, s3Cfg)
			if err == nil {
				for _, key := range backupObjectKeys(found) {
					_ = objectStore.Delete(ctx, key)
				}
			}
		}
	}

	return s.saveRecordsLocked(ctx, remaining)
}

// GetBackupDownloadURL 获取备份文件预签名下载 URL
func (s *BackupService) GetBackupDownloadURL(ctx context.Context, backupID string) (string, error) {
	response, err := s.GetBackupDownloadResponse(ctx, backupID)
	if err != nil {
		return "", err
	}
	return response.URL, nil
}

func (s *BackupService) GetBackupDownloadResponse(ctx context.Context, backupID string) (*BackupDownloadResponse, error) {
	record, err := s.GetBackupRecord(ctx, backupID)
	if err != nil {
		return nil, err
	}
	if record.Status != "completed" {
		return nil, infraerrors.BadRequest("BACKUP_NOT_COMPLETED", "backup is not completed")
	}

	s3Cfg, err := s.loadS3Config(ctx)
	if err != nil {
		return nil, err
	}
	objectStore, err := s.getOrCreateStore(ctx, s3Cfg)
	if err != nil {
		return nil, err
	}
	if len(record.Parts) > 0 {
		parts := make([]BackupDownloadPart, 0, len(record.Parts))
		for _, part := range record.Parts {
			url, err := objectStore.PresignURL(ctx, part.S3Key, time.Hour)
			if err != nil {
				return nil, fmt.Errorf("presign part %d: %w", part.Index, err)
			}
			parts = append(parts, BackupDownloadPart{Index: part.Index, SizeBytes: part.SizeBytes, URL: url})
		}
		return &BackupDownloadResponse{Parts: parts}, nil
	}

	url, err := objectStore.PresignURL(ctx, record.S3Key, 1*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("presign url: %w", err)
	}
	return &BackupDownloadResponse{URL: url}, nil
}

// ─── 内部方法 ───

func (s *BackupService) loadS3Config(ctx context.Context) (*BackupS3Config, error) {
	cfg, err := s.loadStoredS3Config(ctx)
	if err != nil || cfg == nil {
		return cfg, err
	}
	if cfg.SecretAccessKey != "" {
		decrypted, legacyPlaintext, err := decryptStoredSecret(s.encryptor, cfg.SecretAccessKey, cfg.SecretEncryptionVersion)
		if err != nil {
			return nil, fmt.Errorf("decrypt backup S3 secret: %w", err)
		}
		if legacyPlaintext {
			logger.LegacyPrintf("service.backup", "[Backup] using legacy plaintext S3 SecretAccessKey")
		}
		cfg.SecretAccessKey = decrypted
	}
	return cfg, nil
}

func (s *BackupService) loadStoredS3Config(ctx context.Context) (*BackupS3Config, error) {
	raw, err := s.settingRepo.GetValue(ctx, settingKeyBackupS3Config)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil //nolint:nilnil // no config is a valid state
	}
	var cfg BackupS3Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, ErrBackupS3ConfigCorrupt
	}
	return &cfg, nil
}

func (s *BackupService) getOrCreateStore(ctx context.Context, cfg *BackupS3Config) (BackupObjectStore, error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()

	if s.store != nil && s.s3Cfg != nil {
		return s.store, nil
	}

	if cfg == nil {
		return nil, ErrBackupS3NotConfigured
	}

	store, err := s.storeFactory(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s.store = store
	s.s3Cfg = cfg
	return store, nil
}

func (s *BackupService) buildS3Key(cfg *BackupS3Config, fileName string) string {
	prefix := strings.TrimRight(cfg.Prefix, "/")
	if prefix == "" {
		prefix = "backups"
	}
	return fmt.Sprintf("%s/%s/%s", prefix, time.Now().Format("2006/01/02"), fileName)
}

// loadRecords 加载备份记录，区分"无数据"和"数据损坏"
func (s *BackupService) loadRecords(ctx context.Context) ([]BackupRecord, error) {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()
	return s.loadRecordsLocked(ctx)
}

// loadRecordsLocked 在已持有 recordsMu 锁的情况下加载记录
func (s *BackupService) loadRecordsLocked(ctx context.Context) ([]BackupRecord, error) {
	raw, err := s.settingRepo.GetValue(ctx, settingKeyBackupRecords)
	if err != nil || raw == "" {
		return nil, nil //nolint:nilnil // no records is a valid state
	}
	var records []BackupRecord
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		return nil, ErrBackupRecordsCorrupt
	}
	return records, nil
}

// saveRecordsLocked 在已持有 recordsMu 锁的情况下保存记录
func (s *BackupService) saveRecordsLocked(ctx context.Context, records []BackupRecord) error {
	data, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return s.settingRepo.Set(ctx, settingKeyBackupRecords, string(data))
}

// saveRecord 保存单条记录（带互斥锁保护）
func (s *BackupService) saveRecord(ctx context.Context, record *BackupRecord) error {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()

	records, err := s.loadRecordsLocked(ctx)
	if err != nil {
		return err
	}

	// 更新已有记录或追加
	found := false
	for i := range records {
		if records[i].ID == record.ID {
			records[i] = *record
			found = true
			break
		}
	}
	if !found {
		records = append(records, *record)
	}

	// 限制记录数量
	if len(records) > maxBackupRecords {
		records = records[len(records)-maxBackupRecords:]
	}

	return s.saveRecordsLocked(ctx, records)
}

func (s *BackupService) cleanupOldBackups(ctx context.Context, schedule *BackupScheduleConfig) error {
	if schedule == nil {
		return nil
	}

	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()

	records, err := s.loadRecordsLocked(ctx)
	if err != nil {
		return err
	}

	// 按时间倒序
	sort.Slice(records, func(i, j int) bool {
		return records[i].StartedAt > records[j].StartedAt
	})

	var toDelete []BackupRecord
	var toKeep []BackupRecord

	for i, r := range records {
		shouldDelete := false

		// 按保留份数清理
		if schedule.RetainCount > 0 && i >= schedule.RetainCount {
			shouldDelete = true
		}

		// 按保留天数清理
		if schedule.RetainDays > 0 && r.StartedAt != "" {
			startedAt, err := time.Parse(time.RFC3339, r.StartedAt)
			if err == nil && time.Since(startedAt) > time.Duration(schedule.RetainDays)*24*time.Hour {
				shouldDelete = true
			}
		}

		if shouldDelete && r.Status == "completed" {
			toDelete = append(toDelete, r)
		} else {
			toKeep = append(toKeep, r)
		}
	}

	// 删除 S3 上的文件
	for _, r := range toDelete {
		for _, key := range backupObjectKeys(&r) {
			_ = s.deleteS3Object(ctx, key)
		}
	}

	if len(toDelete) > 0 {
		logger.LegacyPrintf("service.backup", "[Backup] 自动清理了 %d 个过期备份", len(toDelete))
		return s.saveRecordsLocked(ctx, toKeep)
	}
	return nil
}

func (s *BackupService) deleteS3Object(ctx context.Context, key string) error {
	s3Cfg, err := s.loadS3Config(ctx)
	if err != nil || s3Cfg == nil {
		return nil
	}
	objectStore, err := s.getOrCreateStore(ctx, s3Cfg)
	if err != nil {
		return err
	}
	return objectStore.Delete(ctx, key)
}
