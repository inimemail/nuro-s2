package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type failingMultipartStore struct {
	objects map[string][]byte
	failed  bool
}

type successfulMultipartStore struct {
	objects           map[string][]byte
	deleteCtxCanceled bool
}

func (s *successfulMultipartStore) Upload(context.Context, string, io.Reader, string) (int64, error) {
	return 0, fmt.Errorf("single upload not expected")
}
func (s *successfulMultipartStore) Download(context.Context, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("download not expected")
}
func (s *successfulMultipartStore) Delete(ctx context.Context, key string) error {
	if ctx.Err() != nil {
		s.deleteCtxCanceled = true
	}
	delete(s.objects, key)
	return nil
}
func (s *successfulMultipartStore) PresignURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (s *successfulMultipartStore) HeadBucket(context.Context) error { return nil }
func (s *successfulMultipartStore) UploadFile(_ context.Context, key, path, _ string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s.objects[key] = data
	return int64(len(data)), nil
}

func (s *failingMultipartStore) Upload(context.Context, string, io.Reader, string) (int64, error) {
	return 0, fmt.Errorf("single upload not expected")
}
func (s *failingMultipartStore) Download(context.Context, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("download not expected")
}
func (s *failingMultipartStore) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}
func (s *failingMultipartStore) PresignURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (s *failingMultipartStore) HeadBucket(context.Context) error { return nil }
func (s *failingMultipartStore) UploadFile(_ context.Context, key, path, _ string) (int64, error) {
	if s.failed {
		return 0, fmt.Errorf("multipart upload failed")
	}
	s.failed = true
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s.objects[key] = data
	return int64(len(data)), nil
}

func TestSplitBackupFileProducesBoundedPartsAndHashes(t *testing.T) {
	f, err := os.CreateTemp("", "backup-archive-test-*")
	require.NoError(t, err)
	path := f.Name()
	defer os.Remove(path)
	_, err = f.Write([]byte("0123456789abcdef"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	parts, err := splitBackupFile(path, 5)
	require.NoError(t, err)
	defer cleanupBackupPartFiles(parts)
	require.Len(t, parts, 4)
	require.EqualValues(t, 5, parts[0].SizeBytes)
	require.EqualValues(t, 1, parts[3].SizeBytes)
	for _, part := range parts {
		info, statErr := os.Stat(part.Path)
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		require.Len(t, part.SHA256, 64)
	}
}

func TestUploadBackupArchive_CleansPreviouslyUploadedPartsOnFailure(t *testing.T) {
	f, err := os.CreateTemp("", "backup-archive-upload-test-*")
	require.NoError(t, err)
	path := f.Name()
	defer os.Remove(path)
	_, err = f.Write([]byte("0123456789abcdef"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	store := &failingMultipartStore{objects: make(map[string][]byte)}
	svc := &BackupService{partSizeBytes: 5}
	record := &BackupRecord{S3Key: "backups/test.sql.gz"}
	err = svc.uploadBackupArchive(context.Background(), record, store, path, nil)
	require.Error(t, err)
	require.Empty(t, store.objects, "failed multipart upload must not leave orphaned parts")
	require.Empty(t, record.Parts, "failed multipart upload must not retain deleted parts")
}

func TestUploadBackupArchive_PersistsEachUploadedPart(t *testing.T) {
	path := writeBackupArchiveFixture(t)
	store := &successfulMultipartStore{objects: make(map[string][]byte)}
	svc := &BackupService{partSizeBytes: 5}
	record := &BackupRecord{S3Key: "backups/test.sql.gz"}
	persistedCounts := make([]int, 0, 4)

	err := svc.uploadBackupArchive(context.Background(), record, store, path, func() error {
		persistedCounts = append(persistedCounts, len(record.Parts))
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []int{1, 2, 3, 4}, persistedCounts)
	require.Len(t, record.Parts, 4)
	require.Len(t, store.objects, 4)
}

func TestUploadBackupArchive_UsesFileUploadForSingleObject(t *testing.T) {
	path := writeBackupArchiveFixture(t)
	store := &successfulMultipartStore{objects: make(map[string][]byte)}
	svc := &BackupService{partSizeBytes: 1 << 20}
	record := &BackupRecord{S3Key: "backups/test.sql.gz"}

	err := svc.uploadBackupArchive(context.Background(), record, store, path, nil)

	require.NoError(t, err)
	require.Contains(t, store.objects, record.S3Key)
	require.Empty(t, record.Parts)
}

func TestUploadBackupArchive_CleansPartsWhenProgressPersistenceFails(t *testing.T) {
	path := writeBackupArchiveFixture(t)
	store := &successfulMultipartStore{objects: make(map[string][]byte)}
	svc := &BackupService{partSizeBytes: 5}
	record := &BackupRecord{S3Key: "backups/test.sql.gz"}
	persistCalls := 0

	err := svc.uploadBackupArchive(context.Background(), record, store, path, func() error {
		persistCalls++
		if persistCalls == 2 {
			return fmt.Errorf("record storage unavailable")
		}
		return nil
	})
	require.ErrorContains(t, err, "persist backup part 2")
	require.Empty(t, record.Parts)
	require.Empty(t, store.objects)
}

func TestUploadBackupArchive_CleanupDoesNotReuseCanceledRequestContext(t *testing.T) {
	path := writeBackupArchiveFixture(t)
	store := &successfulMultipartStore{objects: make(map[string][]byte)}
	svc := &BackupService{partSizeBytes: 5}
	record := &BackupRecord{S3Key: "backups/test.sql.gz"}
	ctx, cancel := context.WithCancel(context.Background())

	err := svc.uploadBackupArchive(ctx, record, store, path, func() error {
		cancel()
		return fmt.Errorf("record storage unavailable")
	})
	require.Error(t, err)
	require.False(t, store.deleteCtxCanceled)
	require.Empty(t, store.objects)
}

func writeBackupArchiveFixture(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "backup-archive-upload-test-*")
	require.NoError(t, err)
	path := f.Name()
	t.Cleanup(func() { _ = os.Remove(path) })
	_, err = f.Write([]byte("0123456789abcdef"))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return path
}
