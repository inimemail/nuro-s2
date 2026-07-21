package repository

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type imageStorageS3 struct {
	store         service.BackupObjectStore
	publicBaseURL string
	presignExpiry time.Duration
}

func ProvideImageStorageFactory(storeFactory service.BackupObjectStoreFactory) service.ImageStorageFactory {
	return func(ctx context.Context, cfg *config.ImageStorageConfig) (service.ImageStorage, error) {
		if cfg == nil || !cfg.Active() {
			return nil, service.ErrImageStorageIncomplete
		}
		store, err := storeFactory(ctx, &service.BackupS3Config{
			Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket,
			AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey,
			ForcePathStyle: cfg.ForcePathStyle,
		})
		if err != nil {
			return nil, err
		}
		expiry := time.Duration(cfg.PresignExpiry) * time.Hour
		if expiry <= 0 {
			expiry = 24 * time.Hour
		}
		return &imageStorageS3{store: store, publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/"), presignExpiry: expiry}, nil
	}
}

func (s *imageStorageS3) Save(ctx context.Context, key, contentType string, data []byte) (string, error) {
	if _, err := s.store.Upload(ctx, key, bytes.NewReader(data), contentType); err != nil {
		return "", err
	}
	if s.publicBaseURL != "" {
		return s.publicBaseURL + "/" + strings.TrimLeft(key, "/"), nil
	}
	return s.store.PresignURL(ctx, key, s.presignExpiry)
}
