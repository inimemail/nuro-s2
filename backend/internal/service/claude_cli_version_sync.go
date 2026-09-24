package service

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/google/uuid"
)

const (
	SettingKeyClaudeCLIClientVersion          = "claude_cli_client_version"
	SettingKeyClaudeCLIClientVersionSynced    = "claude_cli_client_version_synced"
	SettingKeyClaudeCLIVersionAutoSyncEnabled = "claude_cli_version_auto_sync_enabled"
)

type claudeCLIIdentity struct{ version, source string }

var claudeCLIIdentitySnapshot atomic.Pointer[claudeCLIIdentity]

type claudeCLIIdentityContextKey struct{}

func normalizeClaudeCLIVersion(value string) string {
	value = strings.TrimSpace(value)
	if !claude.IsSupportedCLIVersion(value) {
		return ""
	}
	return value
}
func resolveClaudeCLIIdentity(manual, synced string, enabled bool) *claudeCLIIdentity {
	if version := normalizeClaudeCLIVersion(manual); version != "" {
		return &claudeCLIIdentity{version, "manual"}
	}
	if enabled {
		if version := normalizeClaudeCLIVersion(synced); version != "" {
			return &claudeCLIIdentity{version, "synced"}
		}
	}
	return &claudeCLIIdentity{claude.CLIVersion(), "builtin"}
}
func currentClaudeCLIIdentity() *claudeCLIIdentity {
	if value := claudeCLIIdentitySnapshot.Load(); value != nil {
		return value
	}
	return resolveClaudeCLIIdentity("", "", false)
}
func captureClaudeCLIIdentity(ctx context.Context) context.Context {
	if _, ok := ctx.Value(claudeCLIIdentityContextKey{}).(*claudeCLIIdentity); ok {
		return ctx
	}
	return context.WithValue(ctx, claudeCLIIdentityContextKey{}, currentClaudeCLIIdentity())
}
func claudeCLIVersionForContext(ctx context.Context) string {
	if value, ok := ctx.Value(claudeCLIIdentityContextKey{}).(*claudeCLIIdentity); ok {
		return value.version
	}
	return currentClaudeCLIIdentity().version
}
func claudeCLIVersionArgument(versions []string) string {
	if len(versions) > 0 && versions[0] != "" {
		return versions[0]
	}
	return currentClaudeCLIIdentity().version
}

type claudeVersionCASRepository interface {
	CompareAndSwapClaudeSyncedVersion(context.Context, string, string) (bool, error)
}

// Refresh runs on the existing control-plane identity refresh tick. Disabled
// instances create no Claude timer and perform no release requests.
type ClaudeCLIVersionSyncService struct {
	repo        SettingRepository
	github      GitHubReleaseClient
	lock        LeaderLockCache
	db          *sql.DB
	mu          sync.Mutex
	revision    uint64
	nextAttempt time.Time
	cancel      context.CancelFunc
	stopped     bool
	wg          sync.WaitGroup
}

func (s *ClaudeCLIVersionSyncService) Apply(manual, synced string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLocked(manual, synced, enabled)
}
func (s *ClaudeCLIVersionSyncService) applyLocked(manual, synced string, enabled bool) {
	s.revision++
	next := resolveClaudeCLIIdentity(manual, synced, enabled)
	previous := currentClaudeCLIIdentity()
	// An in-flight settings save may carry a stale read-only synced value.
	if enabled && normalizeClaudeCLIVersion(manual) == "" && previous.source == "synced" && (next.source != "synced" || CompareVersions(next.version, previous.version) < 0) {
		next = previous
	}
	claudeCLIIdentitySnapshot.Store(next)
	if !enabled && s.cancel != nil {
		s.cancel()
	}
}
func (s *ClaudeCLIVersionSyncService) Refresh(ctx context.Context) {
	if s == nil || s.repo == nil {
		return
	}
	s.mu.Lock()
	revision := s.revision
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	values, err := s.repo.GetMultiple(ctx, []string{SettingKeyClaudeCLIClientVersion, SettingKeyClaudeCLIClientVersionSynced, SettingKeyClaudeCLIVersionAutoSyncEnabled})
	if err != nil {
		s.mu.Lock()
		if s.revision == revision && s.cancel != nil {
			s.cancel()
		}
		// Retain the last known valid identity; storage failure never starts a sync.
		s.mu.Unlock()
		slog.Warn("claude_cli_settings_refresh_failed", "error", err)
		return
	}
	enabled := values[SettingKeyClaudeCLIVersionAutoSyncEnabled] == "true"
	s.mu.Lock()
	if s.revision != revision {
		s.mu.Unlock()
		return
	}
	s.applyLocked(values[SettingKeyClaudeCLIClientVersion], values[SettingKeyClaudeCLIClientVersionSynced], enabled)
	if !enabled || s.stopped || s.cancel != nil || time.Now().Before(s.nextAttempt) {
		s.mu.Unlock()
		return
	}
	workerCtx, workerCancel := context.WithTimeout(context.Background(), 30*time.Second)
	s.cancel = workerCancel
	s.nextAttempt = time.Now().Add(time.Hour)
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer workerCancel()
		defer func() { s.mu.Lock(); s.cancel = nil; s.mu.Unlock() }()
		s.syncOnce(workerCtx)
	}()
}
func (s *ClaudeCLIVersionSyncService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
func latestClaudeStableVersion(releases []*GitHubRelease) string {
	best := ""
	for _, r := range releases {
		if r == nil || r.Draft || r.Prerelease {
			continue
		}
		version := normalizeClaudeCLIVersion(strings.TrimPrefix(strings.TrimSpace(r.TagName), "v"))
		if version != "" && (best == "" || CompareVersions(version, best) > 0) {
			best = version
		}
	}
	return best
}
func (s *ClaudeCLIVersionSyncService) syncOnce(ctx context.Context) {
	if s.github == nil {
		return
	}
	release, ok := tryAcquireFixedLeaderLock(ctx, s.lock, s.db, "claude-cli:version-sync", uuid.NewString(), 2*time.Minute)
	if !ok {
		return
	}
	defer release()
	current, err := s.repo.GetValue(ctx, SettingKeyClaudeCLIClientVersionSynced)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return
	}
	latest, err := s.github.FetchLatestRelease(ctx, "anthropics/claude-code")
	version := ""
	if err == nil {
		version = latestClaudeStableVersion([]*GitHubRelease{latest})
	}
	if version == "" {
		releases, err := s.github.FetchRecentReleases(ctx, "anthropics/claude-code", 30)
		if err != nil {
			return
		}
		version = latestClaudeStableVersion(releases)
	}
	if version == "" || (current != "" && CompareVersions(version, current) <= 0) || ctx.Err() != nil {
		return
	}
	repo, ok := s.repo.(claudeVersionCASRepository)
	if !ok {
		return
	}
	saved, err := repo.CompareAndSwapClaudeSyncedVersion(ctx, current, version)
	if err != nil {
		slog.Warn("claude_cli_version_sync_failed", "error", err)
		return
	}
	if saved {
		s.Refresh(ctx)
		slog.Info("claude_cli_version_synced", "version", version)
	}
}

func IsValidClaudeCLIVersion(value string) bool { return normalizeClaudeCLIVersion(value) != "" }
