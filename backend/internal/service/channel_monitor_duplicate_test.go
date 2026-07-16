//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

type duplicateChannelMonitorRepoStub struct {
	ChannelMonitorRepository
	source      *ChannelMonitor
	created     []*ChannelMonitor
	byOperation map[string]*ChannelMonitor
}

func (r *duplicateChannelMonitorRepoStub) GetByID(_ context.Context, id int64) (*ChannelMonitor, error) {
	if r.source == nil || r.source.ID != id {
		return nil, ErrChannelMonitorNotFound
	}
	return r.source, nil
}

func (r *duplicateChannelMonitorRepoStub) Create(_ context.Context, monitor *ChannelMonitor) error {
	monitor.ID = int64(100 + len(r.created) + 1)
	monitor.CreatedAt = time.Date(2026, time.July, 16, 8, 0, 0, 0, time.UTC)
	monitor.UpdatedAt = monitor.CreatedAt
	stored := *monitor
	stored.ExtraModels = append([]string(nil), monitor.ExtraModels...)
	stored.ExtraHeaders = cloneChannelMonitorHeaders(monitor.ExtraHeaders)
	stored.BodyOverride, _ = cloneChannelMonitorJSONMap(monitor.BodyOverride)
	r.created = append(r.created, &stored)
	if stored.DuplicateOperationID != "" {
		if r.byOperation == nil {
			r.byOperation = make(map[string]*ChannelMonitor)
		}
		r.byOperation[stored.DuplicateOperationID] = &stored
	}
	return nil
}

func (r *duplicateChannelMonitorRepoStub) FindByDuplicateOperationID(_ context.Context, operationID string) (*ChannelMonitor, error) {
	monitor := r.byOperation[operationID]
	if monitor == nil {
		return nil, nil
	}
	cloned := *monitor
	cloned.ExtraModels = append([]string(nil), monitor.ExtraModels...)
	cloned.ExtraHeaders = cloneChannelMonitorHeaders(monitor.ExtraHeaders)
	cloned.BodyOverride, _ = cloneChannelMonitorJSONMap(monitor.BodyOverride)
	return &cloned, nil
}

type duplicateChannelMonitorEncryptor struct{ decryptErr error }

func (e *duplicateChannelMonitorEncryptor) Encrypt(plaintext string) (string, error) {
	return "NEW:" + plaintext, nil
}

func (e *duplicateChannelMonitorEncryptor) Decrypt(ciphertext string) (string, error) {
	if e.decryptErr != nil {
		return "", e.decryptErr
	}
	if !strings.HasPrefix(ciphertext, "OLD:") && !strings.HasPrefix(ciphertext, "NEW:") {
		return "", errors.New("invalid ciphertext")
	}
	return strings.TrimPrefix(strings.TrimPrefix(ciphertext, "OLD:"), "NEW:"), nil
}

func TestDuplicateChannelMonitorCopiesConfigAndResetsRuntimeState(t *testing.T) {
	lastCheckedAt := time.Date(2026, time.July, 15, 7, 0, 0, 0, time.UTC)
	templateID := int64(9)
	source := &ChannelMonitor{
		ID: 42, Name: "primary", Provider: MonitorProviderOpenAI,
		APIMode: MonitorAPIModeResponses, Endpoint: "https://api.example.com",
		APIKey: "OLD:top-secret", PrimaryModel: "gpt-5.4-mini",
		ExtraModels: []string{"gpt-5.4"}, GroupName: "production", Enabled: true,
		IntervalSeconds: 90, LastCheckedAt: &lastCheckedAt, CreatedBy: 4,
		TemplateID: &templateID, ExtraHeaders: map[string]string{"User-Agent": "Codex"},
		BodyOverrideMode: MonitorBodyOverrideModeMerge,
		BodyOverride:     map[string]any{"metadata": map[string]any{"source": "original"}},
	}
	repo := &duplicateChannelMonitorRepoStub{source: source}
	svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{})

	duplicate, err := svc.Duplicate(context.Background(), 42, 77, "admin:77", "copy-primary")

	require.NoError(t, err)
	require.Len(t, repo.created, 1)
	require.Equal(t, "NEW:top-secret", repo.created[0].APIKey)
	require.Equal(t, "top-secret", duplicate.APIKey)
	require.Equal(t, "primary (Copy)", duplicate.Name)
	require.False(t, duplicate.Enabled)
	require.Nil(t, duplicate.LastCheckedAt)
	require.Equal(t, int64(77), duplicate.CreatedBy)
	require.NotEmpty(t, duplicate.DuplicateOperationID)
	require.Equal(t, source.ExtraModels, duplicate.ExtraModels)
	require.Equal(t, source.ExtraHeaders, duplicate.ExtraHeaders)
	require.Equal(t, source.BodyOverride, duplicate.BodyOverride)

	duplicate.ExtraModels[0] = "changed"
	duplicate.ExtraHeaders["User-Agent"] = "changed"
	duplicate.BodyOverride["metadata"].(map[string]any)["source"] = "changed"
	require.Equal(t, "gpt-5.4", source.ExtraModels[0])
	require.Equal(t, "Codex", source.ExtraHeaders["User-Agent"])
	require.Equal(t, "original", source.BodyOverride["metadata"].(map[string]any)["source"])
}

func TestDuplicateChannelMonitorRecoversSameOperation(t *testing.T) {
	source := &ChannelMonitor{ID: 42, Name: "primary", APIKey: "OLD:top-secret"}
	repo := &duplicateChannelMonitorRepoStub{source: source}
	svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{})

	first, err := svc.Duplicate(context.Background(), 42, 77, "admin:77", "stable-key")
	require.NoError(t, err)
	retry, err := svc.Duplicate(context.Background(), 42, 77, "admin:77", "stable-key")
	require.NoError(t, err)

	require.Len(t, repo.created, 1)
	require.Equal(t, first.ID, retry.ID)
	require.Equal(t, "top-secret", retry.APIKey)
	require.NotContains(t, retry.ExtraHeaders, ChannelMonitorDuplicateOperationIDMetadataKey)

	_, err = svc.Duplicate(context.Background(), 42, 88, "admin:88", "stable-key")
	require.NoError(t, err)
	require.Len(t, repo.created, 2)
}

func TestDuplicateChannelMonitorRejectsUndecryptableKey(t *testing.T) {
	repo := &duplicateChannelMonitorRepoStub{source: &ChannelMonitor{ID: 42, Name: "broken", APIKey: "OLD:broken"}}
	svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{decryptErr: errors.New("wrong key")})

	duplicate, err := svc.Duplicate(context.Background(), 42, 77, "admin:77", "copy-broken")

	require.Nil(t, duplicate)
	require.ErrorIs(t, err, ErrChannelMonitorAPIKeyDecryptFailed)
	require.Empty(t, repo.created)
}

func TestDuplicateChannelMonitorNameFitsSchemaLimit(t *testing.T) {
	name := duplicateChannelMonitorName(strings.Repeat("界", 100))
	require.Equal(t, 100, utf8.RuneCountInString(name))
	require.True(t, strings.HasSuffix(name, " (Copy)"))
}
