package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const settingKeyImageStorageConfig = "image_storage_config"

var ErrImageStorageIncomplete = errors.New("image storage is enabled but bucket/access_key_id/secret_access_key are incomplete")

type ImageStorageFactory func(context.Context, *config.ImageStorageConfig) (ImageStorage, error)

type ImageStorageSettings struct {
	Enabled                 bool   `json:"enabled"`
	ReuseBackupS3           bool   `json:"reuse_backup_s3"`
	Bucket                  string `json:"bucket"`
	Prefix                  string `json:"prefix"`
	PublicBaseURL           string `json:"public_base_url"`
	PresignExpiry           int    `json:"presign_expiry_hours"`
	MaxDownloadBytes        int64  `json:"max_download_bytes"`
	Endpoint                string `json:"endpoint"`
	Region                  string `json:"region"`
	AccessKeyID             string `json:"access_key_id"`
	SecretAccessKey         string `json:"secret_access_key,omitempty"`
	SecretEncryptionVersion int    `json:"secret_encryption_version,omitempty"`
	ForcePathStyle          bool   `json:"force_path_style"`
}

type ImageStorageSettingService struct {
	settingRepo SettingRepository
	encryptor   SecretEncryptor
	backup      *BackupService
	factory     ImageStorageFactory
	fallback    config.ImageStorageConfig
	mu          sync.Mutex
	resolved    bool
	uploader    *ImageResultUploader
	enabled     bool
}

func NewImageStorageSettingService(repo SettingRepository, encryptor SecretEncryptor, backup *BackupService, factory ImageStorageFactory, fallback config.ImageStorageConfig) *ImageStorageSettingService {
	service := &ImageStorageSettingService{settingRepo: repo, encryptor: encryptor, backup: backup, factory: factory, fallback: fallback}
	if backup != nil {
		backup.SetS3ConfigInvalidator(service.Invalidate)
	}
	return service
}

func (s *ImageStorageSettingService) Resolver() ImageStorageResolver {
	return func() (*ImageResultUploader, bool) { return s.resolve() }
}

type ImageStorageResolver func() (*ImageResultUploader, bool)

func (s *ImageStorageSettingService) resolve() (*ImageResultUploader, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolved {
		return s.uploader, s.enabled
	}
	s.uploader = nil
	s.enabled = false
	cfg, err := s.effectiveConfig(context.Background())
	if err != nil {
		slog.Warn("image storage configuration could not be resolved; retrying on next use", "error", err)
		return nil, false
	}
	if cfg == nil || !cfg.Active() {
		s.resolved = true
		return nil, false
	}
	if s.factory == nil {
		return nil, false
	}
	storage, err := s.factory(context.Background(), cfg)
	if err != nil {
		slog.Warn("image storage client could not be created; retrying on next use", "error", err)
		return nil, false
	}
	s.uploader = NewImageResultUploader(storage, cfg.Prefix, cfg.MaxImageBytes)
	s.enabled = true
	s.resolved = true
	return s.uploader, true
}

func (s *ImageStorageSettingService) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.resolved = false
	s.uploader = nil
	s.enabled = false
	s.mu.Unlock()
}

func (s *ImageStorageSettingService) Get(ctx context.Context) (*ImageStorageSettings, error) {
	v, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if v == nil {
		v = settingsFromImageConfig(s.fallback)
	}
	v.SecretAccessKey = ""
	v.SecretEncryptionVersion = 0
	return v, nil
}
func (s *ImageStorageSettingService) SecretConfigured(ctx context.Context) bool {
	v, err := s.load(ctx)
	if err != nil || v == nil {
		return s.fallback.SecretAccessKey != ""
	}
	if v.ReuseBackupS3 {
		b, e := s.backupCredentials(ctx)
		return e == nil && b != nil && b.SecretAccessKey != ""
	}
	return v.SecretAccessKey != ""
}

func (s *ImageStorageSettingService) Update(ctx context.Context, in ImageStorageSettings) (*ImageStorageSettings, error) {
	normalizeImageStorageSettings(&in)
	in.SecretEncryptionVersion = 0
	if in.ReuseBackupS3 {
		in.Endpoint, in.Region, in.AccessKeyID, in.SecretAccessKey = "", "", "", ""
		in.ForcePathStyle = false
	} else if in.SecretAccessKey == "" {
		old, e := s.load(ctx)
		if e != nil {
			return nil, e
		}
		if old != nil {
			in.SecretAccessKey = old.SecretAccessKey
			in.SecretEncryptionVersion = old.SecretEncryptionVersion
		}
	} else {
		if s.backup == nil || !s.backup.EncryptionKeyConfigured() {
			return nil, ErrSecretEncryptionKeyNotConfigured
		}
		if s.encryptor == nil {
			return nil, ErrSecretEncryptionKeyNotConfigured
		}
		enc, e := encryptStoredSecret(s.encryptor, in.SecretAccessKey)
		if e != nil {
			return nil, fmt.Errorf("encrypt image storage secret: %w", e)
		}
		in.SecretAccessKey = enc
		in.SecretEncryptionVersion = storedSecretEncryptionVersion
	}
	raw, e := json.Marshal(in)
	if e != nil {
		return nil, e
	}
	if s.settingRepo == nil {
		return nil, errors.New("settings repository unavailable")
	}
	if e = s.settingRepo.Set(ctx, settingKeyImageStorageConfig, string(raw)); e != nil {
		return nil, e
	}
	s.Invalidate()
	in.SecretAccessKey = ""
	in.SecretEncryptionVersion = 0
	return &in, nil
}

func (s *ImageStorageSettingService) TestConnection(ctx context.Context, in ImageStorageSettings) error {
	normalizeImageStorageSettings(&in)
	in.SecretEncryptionVersion = 0
	if !in.ReuseBackupS3 && in.SecretAccessKey == "" {
		old, e := s.load(ctx)
		if e != nil {
			return e
		}
		if old != nil {
			in.SecretAccessKey = old.SecretAccessKey
			in.SecretEncryptionVersion = old.SecretEncryptionVersion
		}
	}
	cfg, e := s.toConfig(ctx, &in)
	if e != nil {
		return e
	}
	if cfg == nil || !cfg.Active() {
		return ErrImageStorageIncomplete
	}
	if s.factory == nil {
		return errors.New("image storage factory unavailable")
	}
	_, e = s.factory(ctx, cfg)
	return e
}

func (s *ImageStorageSettingService) effectiveConfig(ctx context.Context) (*config.ImageStorageConfig, error) {
	v, e := s.load(ctx)
	if e != nil {
		return nil, e
	}
	if v == nil {
		c := s.fallback
		return &c, nil
	}
	return s.toConfig(ctx, v)
}
func (s *ImageStorageSettingService) toConfig(ctx context.Context, in *ImageStorageSettings) (*config.ImageStorageConfig, error) {
	c := &config.ImageStorageConfig{Enabled: in.Enabled, Bucket: in.Bucket, Prefix: in.Prefix, PublicBaseURL: in.PublicBaseURL, PresignExpiry: in.PresignExpiry, MaxImageBytes: in.MaxDownloadBytes, Endpoint: in.Endpoint, Region: in.Region, AccessKeyID: in.AccessKeyID, SecretAccessKey: in.SecretAccessKey, ForcePathStyle: in.ForcePathStyle}
	if in.ReuseBackupS3 {
		b, e := s.backupCredentials(ctx)
		if e != nil {
			return nil, e
		}
		if b == nil {
			return nil, ErrBackupS3NotConfigured
		}
		c.Endpoint, c.Region, c.AccessKeyID, c.SecretAccessKey, c.ForcePathStyle = b.Endpoint, b.Region, b.AccessKeyID, b.SecretAccessKey, b.ForcePathStyle
		if c.Bucket == "" {
			c.Bucket = b.Bucket
		}
	} else if c.SecretAccessKey != "" {
		if s.encryptor == nil {
			slog.Warn("image storage secret decryptor unavailable; using legacy plaintext compatibility")
		}
		dec, legacyPlaintext, e := decryptStoredSecret(s.encryptor, c.SecretAccessKey, in.SecretEncryptionVersion)
		if e != nil {
			return nil, fmt.Errorf("decrypt image storage secret: %w", e)
		}
		c.SecretAccessKey = dec
		if legacyPlaintext {
			// Older deployments could persist plaintext before encryption was
			// introduced. Keep that data usable, but make the fallback auditable
			// without ever logging the stored value.
			slog.Warn("image storage secret decrypt failed; using legacy plaintext compatibility")
		}
	}
	return c, nil
}
func (s *ImageStorageSettingService) backupCredentials(ctx context.Context) (*BackupS3Config, error) {
	if s.backup == nil {
		return nil, ErrBackupS3NotConfigured
	}
	return s.backup.LoadS3ConfigForImage(ctx)
}
func (s *ImageStorageSettingService) load(ctx context.Context) (*ImageStorageSettings, error) {
	if s.settingRepo == nil {
		return nil, nil
	}
	raw, e := s.settingRepo.GetValue(ctx, settingKeyImageStorageConfig)
	if e != nil {
		return nil, e
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var v ImageStorageSettings
	if e = json.Unmarshal([]byte(raw), &v); e != nil {
		return nil, e
	}
	return &v, nil
}
func settingsFromImageConfig(c config.ImageStorageConfig) *ImageStorageSettings {
	return &ImageStorageSettings{Enabled: c.Enabled, Bucket: c.Bucket, Prefix: c.Prefix, PublicBaseURL: c.PublicBaseURL, PresignExpiry: c.PresignExpiry, MaxDownloadBytes: c.MaxImageBytes, Endpoint: c.Endpoint, Region: c.Region, AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, ForcePathStyle: c.ForcePathStyle}
}
func normalizeImageStorageSettings(v *ImageStorageSettings) {
	v.Bucket = strings.TrimSpace(v.Bucket)
	v.Endpoint = strings.TrimSpace(v.Endpoint)
	v.Region = strings.TrimSpace(v.Region)
	v.AccessKeyID = strings.TrimSpace(v.AccessKeyID)
	v.SecretAccessKey = strings.TrimSpace(v.SecretAccessKey)
	v.PublicBaseURL = strings.TrimRight(strings.TrimSpace(v.PublicBaseURL), "/")
	v.Prefix = strings.TrimSpace(v.Prefix)
	if v.Prefix == "" {
		v.Prefix = "images/"
	}
	if !strings.HasSuffix(v.Prefix, "/") {
		v.Prefix += "/"
	}
	if v.Region == "" {
		v.Region = "auto"
	}
	if v.PresignExpiry <= 0 {
		v.PresignExpiry = 24
	}
	if v.MaxDownloadBytes <= 0 {
		v.MaxDownloadBytes = 32 << 20
	}
}
