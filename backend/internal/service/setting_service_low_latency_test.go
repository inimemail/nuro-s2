package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type settingLowLatencyRepoStub struct {
	updates map[string]string
}

func (s *settingLowLatencyRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *settingLowLatencyRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	panic("unexpected GetValue call")
}

func (s *settingLowLatencyRepoStub) Set(ctx context.Context, key, value string) error {
	panic("unexpected Set call")
}

func (s *settingLowLatencyRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *settingLowLatencyRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	s.updates = make(map[string]string, len(settings))
	for key, value := range settings {
		s.updates[key] = value
	}
	return nil
}

func (s *settingLowLatencyRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (s *settingLowLatencyRepoStub) Delete(ctx context.Context, key string) error {
	panic("unexpected Delete call")
}

func TestSettingService_UpdateSettings_StreamLowLatencyModeNormalizesLegacyFlag(t *testing.T) {
	repo := &settingLowLatencyRepoStub{}
	cfg := &config.Config{}
	svc := NewSettingService(repo, cfg)

	err := svc.UpdateSettings(context.Background(), &SystemSettings{
		StreamLowLatencyMode:    config.StreamLowLatencyModeAggressive,
		LowLatencyStreamHeaders: false,
	})

	require.NoError(t, err)
	require.Equal(t, config.StreamLowLatencyModeAggressive, repo.updates[SettingKeyStreamLowLatencyMode])
	require.Equal(t, "true", repo.updates[SettingKeyLowLatencyStreamHeaders])
	require.Equal(t, config.StreamLowLatencyModeAggressive, cfg.StreamLowLatencyMode())
	require.True(t, cfg.LowLatencyStreamHeadersEnabled())
}

func TestSettingService_UpdateSettings_StreamLowLatencyModeRejectsInvalidValue(t *testing.T) {
	repo := &settingLowLatencyRepoStub{}
	svc := NewSettingService(repo, &config.Config{})

	err := svc.UpdateSettings(context.Background(), &SystemSettings{
		StreamLowLatencyMode: "turbo",
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "stream_low_latency_mode")
	require.Nil(t, repo.updates)
}

func TestSettingService_ParseSettings_StreamLowLatencyModeFallsBackToLegacyFlag(t *testing.T) {
	svc := NewSettingService(&settingLowLatencyRepoStub{}, &config.Config{})

	got := svc.parseSettings(map[string]string{
		SettingKeyLowLatencyStreamHeaders: "true",
	})

	require.Equal(t, config.StreamLowLatencyModeSmart, got.StreamLowLatencyMode)
	require.True(t, got.LowLatencyStreamHeaders)
}

func TestSettingService_ParseSettings_StreamLowLatencyModePrefersExplicitMode(t *testing.T) {
	svc := NewSettingService(&settingLowLatencyRepoStub{}, &config.Config{})

	got := svc.parseSettings(map[string]string{
		SettingKeyStreamLowLatencyMode:    config.StreamLowLatencyModeOff,
		SettingKeyLowLatencyStreamHeaders: "true",
	})

	require.Equal(t, config.StreamLowLatencyModeOff, got.StreamLowLatencyMode)
	require.False(t, got.LowLatencyStreamHeaders)
}
