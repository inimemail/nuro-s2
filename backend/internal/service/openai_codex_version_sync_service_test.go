//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type codexVersionSyncSettingRepoStub struct {
	SettingRepository
	mu        sync.Mutex
	values    map[string]string
	updatedAt time.Time
	writes    []string
}

func (r *codexVersionSyncSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *codexVersionSyncSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		out[key] = r.values[key]
	}
	return out, nil
}

func (r *codexVersionSyncSettingRepoStub) Get(_ context.Context, key string) (*Setting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return nil, ErrSettingNotFound
	}
	return &Setting{Key: key, Value: value, UpdatedAt: r.updatedAt}, nil
}

func (r *codexVersionSyncSettingRepoStub) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	r.writes = append(r.writes, value)
	return nil
}

type codexVersionSyncGitHubStub struct {
	GitHubReleaseClient
	latest      *GitHubRelease
	latestErr   error
	releases    []*GitHubRelease
	latestCalls int
	recentCalls int
}

type blockingCodexVersionSyncGitHubStub struct {
	GitHubReleaseClient
	started chan struct{}
	once    sync.Once
}

func (s *blockingCodexVersionSyncGitHubStub) FetchLatestRelease(ctx context.Context, _ string) (*GitHubRelease, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *blockingCodexVersionSyncGitHubStub) FetchRecentReleases(ctx context.Context, _ string, _ int) ([]*GitHubRelease, error) {
	return nil, ctx.Err()
}

func (s *codexVersionSyncGitHubStub) FetchLatestRelease(context.Context, string) (*GitHubRelease, error) {
	s.latestCalls++
	return s.latest, s.latestErr
}

func (s *codexVersionSyncGitHubStub) FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error) {
	s.recentCalls++
	return s.releases, nil
}

func newCodexVersionSyncTestService(repo *codexVersionSyncSettingRepoStub, github *codexVersionSyncGitHubStub) *OpenAICodexVersionSyncService {
	return NewOpenAICodexVersionSyncService(repo, nil, github, openAICodexVersionSyncInterval)
}

func TestOpenAICodexVersionSyncDisabledByDefault(t *testing.T) {
	repo := &codexVersionSyncSettingRepoStub{values: map[string]string{}}
	github := &codexVersionSyncGitHubStub{latest: &GitHubRelease{TagName: "rust-v0.150.0"}}
	newCodexVersionSyncTestService(repo, github).runOnce()
	require.Zero(t, github.latestCalls)
	require.Zero(t, github.recentCalls)
	require.Empty(t, repo.writes)
}

func TestOpenAICodexVersionSyncLatestFallbackAndNoDowngrade(t *testing.T) {
	t.Run("latest stable", func(t *testing.T) {
		repo := &codexVersionSyncSettingRepoStub{values: map[string]string{
			SettingKeyOpenAICodexVersionAutoSyncEnabled: "true",
		}}
		github := &codexVersionSyncGitHubStub{latest: &GitHubRelease{TagName: "rust-v0.150.0"}}
		newCodexVersionSyncTestService(repo, github).runOnce()
		require.Equal(t, []string{"0.150.0"}, repo.writes)
		require.Zero(t, github.recentCalls)
	})

	t.Run("fallback filters and never downgrades", func(t *testing.T) {
		repo := &codexVersionSyncSettingRepoStub{values: map[string]string{
			SettingKeyOpenAICodexVersionAutoSyncEnabled: "true",
			SettingKeyOpenAICodexClientVersionSynced:    "0.150.0",
		}}
		github := &codexVersionSyncGitHubStub{
			latestErr: errors.New("latest unavailable"),
			releases: []*GitHubRelease{
				{TagName: "rust-v0.999.0", Draft: true},
				{TagName: "rust-v0.151.0-alpha.1", Prerelease: true},
				{TagName: "rusty-v8-v150.0.0"},
				{TagName: "rust-v0.149.0"},
			},
		}
		newCodexVersionSyncTestService(repo, github).runOnce()
		require.Equal(t, 1, github.recentCalls)
		require.Empty(t, repo.writes)
	})
}

func TestOpenAICodexVersionSyncInitialSuppressesRecentValue(t *testing.T) {
	repo := &codexVersionSyncSettingRepoStub{
		values: map[string]string{
			SettingKeyOpenAICodexVersionAutoSyncEnabled: "true",
			SettingKeyOpenAICodexClientVersionSynced:    "0.150.0",
		},
		updatedAt: time.Now().Add(-time.Hour),
	}
	github := &codexVersionSyncGitHubStub{latest: &GitHubRelease{TagName: "rust-v0.151.0"}}
	newCodexVersionSyncTestService(repo, github).runInitial()
	require.Zero(t, github.latestCalls)
}

func TestOpenAICodexVersionSyncStopCancelsInFlightRequest(t *testing.T) {
	repo := &codexVersionSyncSettingRepoStub{values: map[string]string{
		SettingKeyOpenAICodexVersionAutoSyncEnabled: "true",
	}}
	github := &blockingCodexVersionSyncGitHubStub{started: make(chan struct{})}
	svc := NewOpenAICodexVersionSyncService(repo, nil, github, time.Hour)
	svc.Start()

	select {
	case <-github.started:
	case <-time.After(time.Second):
		t.Fatal("version sync did not start")
	}
	done := make(chan struct{})
	go func() {
		svc.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the in-flight GitHub request")
	}
}

func TestOpenAICodexIdentityRuntimePrecedenceAndLegacyNoOp(t *testing.T) {
	t.Cleanup(func() { publishCodexIdentityRuntime("", "", "", false) })

	publishCodexIdentityRuntime("", "", "", false)
	headers := http.Header{
		"User-Agent": {"codex_vscode/1.2.3"},
		"Originator": {"codex_vscode"},
		"Version":    {"1.2.3"},
	}
	enforceCodexIdentityHeaders(headers)
	require.Equal(t, "codex_vscode/1.2.3", headers.Get("User-Agent"))
	require.Equal(t, "codex_vscode", headers.Get("Originator"))
	require.Equal(t, "1.2.3", headers.Get("Version"))
	publishCodexIdentityRuntime("", "", "codex-tui/0.125.0 (Linux; x86_64) (codex-tui; 0.125.0)", false)
	require.Equal(t,
		"codex-tui/0.125.0 (Linux; x86_64) (codex-tui; 0.125.0)",
		currentCodexIdentityRuntime().browserUserAgent,
	)

	publishCodexIdentityRuntime("0.151.0", "0.150.0", "", true)
	headers.Set("User-Agent", "third-party/9.9.9")
	headers.Set("Originator", "third-party")
	headers.Set("Version", "9.9.9")
	enforceCodexIdentityHeaders(headers)
	require.Equal(t, "codex_cli_rs/0.151.0", headers.Get("User-Agent"))
	require.Equal(t, "codex_cli_rs", headers.Get("Originator"))
	require.Equal(t, "0.151.0", headers.Get("Version"))
}

func TestOpenAICodexIdentityRuntimeRefreshUsesControlPlaneOnly(t *testing.T) {
	t.Cleanup(func() { publishCodexIdentityRuntime("", "", "", false) })
	repo := &codexVersionSyncSettingRepoStub{values: map[string]string{
		SettingKeyOpenAICodexClientVersion:       "0.151.0",
		SettingKeyOpenAICodexClientVersionSynced: "0.150.0",
		SettingKeyOpenAICodexUserAgent:           "codex-tui/0.140.0 (Linux; x86_64)",
	}}
	svc := NewSettingService(repo, &config.Config{Gateway: config.GatewayConfig{DisableCodexIdentityEnforcement: true}})
	require.NoError(t, svc.RefreshOpenAICodexIdentityRuntime(context.Background()))
	require.Equal(t, "codex-tui/0.151.0 (Linux; x86_64)", svc.GetOpenAICodexUserAgent(context.Background()))
	publishCodexIdentityRuntime("0.151.0", "", DefaultOpenAICodexUserAgent, false)
	require.Contains(t, currentCodexIdentityRuntime().browserUserAgent, "(codex-tui; 0.151.0)")
}

func TestOpenAICodexIdentityRuntimeRejectsUnsafeBrowserUserAgent(t *testing.T) {
	t.Cleanup(func() { publishCodexIdentityRuntime("", "", "", false) })
	publishCodexIdentityRuntime("0.151.0", "", "codex-tui/0.140.0\r\nX-Injected: true", false)
	require.Equal(t, openai.SetCodexUserAgentVersion(DefaultOpenAICodexUserAgent, "0.151.0"), currentCodexIdentityRuntime().browserUserAgent)

	publishCodexIdentityRuntime("0.151.0", "", strings.Repeat("x", codexBrowserUserAgentMaxLen+1), false)
	require.Equal(t, openai.SetCodexUserAgentVersion(DefaultOpenAICodexUserAgent, "0.151.0"), currentCodexIdentityRuntime().browserUserAgent)
}
