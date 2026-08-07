package service

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	openAICodexVersionSyncInterval     = 6 * time.Hour
	openAICodexVersionSyncTimeout      = 30 * time.Second
	openAICodexIdentityRefreshInterval = time.Minute
	openAICodexVersionSyncRepo         = "openai/codex"
	openAICodexVersionSyncPerPage      = 30
	openAICodexVersionTagPrefix        = "rust-v"
)

// OpenAICodexVersionSyncService refreshes only the control-plane identity
// snapshot. It never participates in a gateway request or first-token path.
type OpenAICodexVersionSyncService struct {
	settingRepo    SettingRepository
	settingService *SettingService
	githubClient   GitHubReleaseClient
	interval       time.Duration
	lifecycleCtx   context.Context
	cancel         context.CancelFunc
	stopCh         chan struct{}
	startOnce      sync.Once
	stopOnce       sync.Once
	wg             sync.WaitGroup
}

func NewOpenAICodexVersionSyncService(
	settingRepo SettingRepository,
	settingService *SettingService,
	githubClient GitHubReleaseClient,
	interval time.Duration,
) *OpenAICodexVersionSyncService {
	lifecycleCtx, cancel := context.WithCancel(context.Background())
	return &OpenAICodexVersionSyncService{
		settingRepo: settingRepo, settingService: settingService,
		githubClient: githubClient, interval: interval,
		lifecycleCtx: lifecycleCtx, cancel: cancel, stopCh: make(chan struct{}),
	}
}

func (s *OpenAICodexVersionSyncService) Start() {
	if s == nil || s.settingRepo == nil || s.githubClient == nil || s.interval <= 0 {
		return
	}
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			syncTicker := time.NewTicker(s.interval)
			defer syncTicker.Stop()
			identityTicker := time.NewTicker(openAICodexIdentityRefreshInterval)
			defer identityTicker.Stop()
			s.runInitial()
			for {
				select {
				case <-syncTicker.C:
					s.runOnce()
				case <-identityTicker.C:
					s.refreshIdentityRuntime()
				case <-s.stopCh:
					return
				}
			}
		}()
	})
}

func (s *OpenAICodexVersionSyncService) refreshIdentityRuntime() {
	if s == nil || s.settingService == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.lifecycleCtx, openAICodexVersionSyncTimeout)
	defer cancel()
	if err := s.settingService.RefreshOpenAICodexIdentityRuntime(ctx); err != nil {
		slog.Warn("openai_codex_identity_runtime_refresh_failed", "error", err)
	}
}

func (s *OpenAICodexVersionSyncService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.cancel()
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *OpenAICodexVersionSyncService) runInitial() {
	ctx, cancel := context.WithTimeout(s.lifecycleCtx, openAICodexVersionSyncTimeout)
	defer cancel()
	if !s.autoSyncEnabled(ctx) || s.syncedWithinInterval(ctx) {
		return
	}
	s.runOnce()
}

func (s *OpenAICodexVersionSyncService) syncedWithinInterval(ctx context.Context) bool {
	setting, err := s.settingRepo.Get(ctx, SettingKeyOpenAICodexClientVersionSynced)
	if err != nil || setting == nil || setting.UpdatedAt.IsZero() || s.interval <= 0 {
		return false
	}
	if NormalizeCodexClientVersion(setting.Value) == "" {
		return false
	}
	age := time.Since(setting.UpdatedAt)
	return age >= 0 && age < s.interval
}

func (s *OpenAICodexVersionSyncService) runOnce() {
	ctx, cancel := context.WithTimeout(s.lifecycleCtx, openAICodexVersionSyncTimeout)
	defer cancel()
	if !s.autoSyncEnabled(ctx) {
		return
	}
	latest := s.fetchLatestStableVersion(ctx)
	if latest == "" {
		return
	}
	current := NormalizeCodexClientVersion(s.currentSyncedVersion(ctx))
	if current != "" && CompareVersions(latest, current) <= 0 {
		return
	}
	if err := s.settingRepo.Set(ctx, SettingKeyOpenAICodexClientVersionSynced, latest); err != nil {
		slog.Warn("openai_codex_version_sync_persist_failed", "version", latest, "error", err)
		return
	}
	if s.settingService != nil {
		if err := s.settingService.RefreshOpenAICodexIdentityRuntime(ctx); err != nil {
			slog.Warn("openai_codex_identity_runtime_refresh_failed", "error", err)
		}
	}
	slog.Info("openai_codex_version_synced", "previous", current, "version", latest)
}

func (s *OpenAICodexVersionSyncService) fetchLatestStableVersion(ctx context.Context) string {
	release, err := s.githubClient.FetchLatestRelease(ctx, openAICodexVersionSyncRepo)
	if err != nil {
		slog.Warn("openai_codex_version_sync_latest_fetch_failed", "error", err)
	} else if version := latestCodexStableReleaseVersion([]*GitHubRelease{release}); version != "" {
		return version
	}
	releases, err := s.githubClient.FetchRecentReleases(ctx, openAICodexVersionSyncRepo, openAICodexVersionSyncPerPage)
	if err != nil {
		slog.Warn("openai_codex_version_sync_fetch_failed", "error", err)
		return ""
	}
	return latestCodexStableReleaseVersion(releases)
}

// Missing, empty, and unreadable values are disabled. This preserves the local
// deployment's current outbound headers until an administrator opts in.
func (s *OpenAICodexVersionSyncService) autoSyncEnabled(ctx context.Context) bool {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyOpenAICodexVersionAutoSyncEnabled)
	return err == nil && strings.TrimSpace(value) == "true"
}

func (s *OpenAICodexVersionSyncService) currentSyncedVersion(ctx context.Context) string {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyOpenAICodexClientVersionSynced)
	if err != nil {
		return ""
	}
	return value
}

func latestCodexStableReleaseVersion(releases []*GitHubRelease) string {
	best := ""
	for _, release := range releases {
		if release == nil || release.Draft || release.Prerelease {
			continue
		}
		tag := strings.TrimSpace(release.TagName)
		if !strings.HasPrefix(tag, openAICodexVersionTagPrefix) {
			continue
		}
		version := NormalizeCodexClientVersion(strings.TrimPrefix(tag, openAICodexVersionTagPrefix))
		if version == "" || strings.Contains(version, "-") {
			continue
		}
		if best == "" || CompareVersions(version, best) > 0 {
			best = version
		}
	}
	return best
}
