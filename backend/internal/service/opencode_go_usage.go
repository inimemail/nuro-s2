package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const (
	SettingKeyOpenCodeGoUsageSettings = "opencode_go_usage_settings"
	OpenCodeGoUsageAutoKey            = "opencode_go_usage_auto_refresh"
	OpenCodeGoUsageSourceKey          = "opencode_go_usage_source"
	OpenCodeGoUsageSnapshotKey        = "opencode_go_usage_snapshot"
	OpenCodeGoUsageScopeKey           = "opencode_go_usage_scope"
	OpenCodeGoUsageIdentityKey        = "opencode_go_usage_identity"
	openCodeGoOfficialUsageURL        = "https://opencode.ai/zen/go/usage"
)

var openCodeGoUsageAutoEnabled atomic.Bool
var ErrOpenCodeGoUsageBusy = infraerrors.TooManyRequests("OPENCODE_USAGE_BUSY", "usage refresh is in progress or was attempted recently; retry after 30 seconds")

type OpenCodeGoUsageSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
	DebounceMinutes int  `json:"debounce_minutes"`
}
type OpenCodeGoUsageSnapshot struct {
	Tiers         []CNQuotaTier `json:"tiers,omitempty"`
	FetchedAt     int64         `json:"fetched_at,omitempty"`
	LastAttemptAt int64         `json:"last_attempt_at"`
	NextRefreshAt int64         `json:"next_refresh_at"`
	FailureCount  int           `json:"failure_count,omitempty"`
	HTTPStatus    int           `json:"http_status,omitempty"`
	Error         string        `json:"error,omitempty"`
}
type OpenCodeGoUsageState struct {
	AccountID     int64                    `json:"account_id"`
	Eligible      bool                     `json:"eligible"`
	AutoRefresh   bool                     `json:"auto_refresh"`
	GlobalEnabled bool                     `json:"global_enabled"`
	Source        string                   `json:"source"`
	Snapshot      *OpenCodeGoUsageSnapshot `json:"snapshot,omitempty"`
}
type openCodeGoUsageRepository interface {
	ListDueOpenCodeGoUsageAccounts(context.Context, time.Time, time.Duration, time.Duration, int) ([]*Account, error)
	FindRecentOpenCodeGoUsage(context.Context, string, int64) (*OpenCodeGoUsageSnapshot, error)
	SaveOpenCodeGoUsageSnapshot(context.Context, *Account, string, *OpenCodeGoUsageSnapshot) (bool, error)
	ConfigureOpenCodeGoUsage(context.Context, *Account, bool, string) (bool, error)
}

func IsOpenCodeGoUsageAccount(a *Account) bool {
	if a == nil || a.Type != AccountTypeAPIKey {
		return false
	}
	if a.Platform == PlatformOpenCodeGo {
		return !a.IsOpenCodeZen() && a.GetCNBillingMode() == CNBillingModeCodingPlan
	}
	switch a.Platform {
	case PlatformOpenAI, PlatformAnthropic, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax:
	default:
		return false
	}
	return isOfficialOpenCodeGoBase(a.GetCredential("base_url"))
}
func isOfficialOpenCodeGoBase(base string) bool {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "opencode.ai") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return false
	}
	switch strings.TrimRight(u.Path, "/") {
	case "/zen/go", "/zen/go/v1", "/zen/go/anthropic":
		return true
	}
	return false
}
func OpenCodeGoUsageSource(a *Account) string {
	if a != nil && a.Platform == PlatformOpenCodeGo && a.Extra[OpenCodeGoUsageSourceKey] == "official" {
		return "official"
	}
	return "configured"
}
func openCodeGoQuotaURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	base = strings.TrimSuffix(base, "/v1")
	base = strings.TrimSuffix(base, "/anthropic")
	return base + "/usage"
}
func openCodeGoUsageURL(a *Account) (string, error) {
	if !IsOpenCodeGoUsageAccount(a) {
		return "", infraerrors.BadRequest("OPENCODE_USAGE_INELIGIBLE", "account is not an OpenCode Go subscription")
	}
	// Non-native accounts qualify only with an exact official Go base URL.
	// Their platform-specific default endpoints are unrelated to this subscription.
	if a.Platform != PlatformOpenCodeGo || OpenCodeGoUsageSource(a) == "official" {
		return openCodeGoOfficialUsageURL, nil
	}
	return openCodeGoConfiguredUsageURL(a), nil
}
func openCodeGoConfiguredUsageURL(a *Account) string {
	protocol := a.GetAPIProtocol()
	if protocol == APIProtocolAdaptive {
		protocol = APIProtocolChatCompletions
	}
	return openCodeGoQuotaURL(a.GetCNProtocolBaseURL(protocol))
}
func OpenCodeGoUsageIdentity(a *Account) string {
	if a == nil {
		return ""
	}
	var proxyVersion any
	if a.Proxy != nil {
		proxyVersion = a.Proxy.UpdatedAt
	}
	raw, _ := json.Marshal([]any{a.Platform, a.Type, a.Credentials, a.ProxyID, proxyVersion, a.Extra["account_mode"], a.Extra[cnBillingModeExtraKey], a.Extra[cnAPIProtocolExtraKey], a.Extra[cnAPIBaseURLsExtraKey], a.Extra[cnAPIBaseURLOverridesExtraKey], a.Extra[openCodeGoProtocolRulesKey], OpenCodeGoUsageSource(a)})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func OpenCodeGoUsageSnapshotForAccount(a *Account) *OpenCodeGoUsageSnapshot {
	if !IsOpenCodeGoUsageAccount(a) || a.Extra[OpenCodeGoUsageIdentityKey] != OpenCodeGoUsageIdentity(a) {
		return nil
	}
	raw, err := json.Marshal(a.Extra[OpenCodeGoUsageSnapshotKey])
	if err != nil {
		return nil
	}
	var value *OpenCodeGoUsageSnapshot
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}
func IsOpenCodeGoUsageManagedKey(key string) bool {
	switch key {
	case OpenCodeGoUsageAutoKey, OpenCodeGoUsageSourceKey, OpenCodeGoUsageSnapshotKey, OpenCodeGoUsageScopeKey, OpenCodeGoUsageIdentityKey:
		return true
	}
	return false
}
func scheduleOpenCodeGoUsageActivity(deferred *DeferredService, a *Account) {
	if !openCodeGoUsageAutoEnabled.Load() || deferred == nil || a == nil || !IsOpenCodeGoUsageAccount(a) || a.Extra[OpenCodeGoUsageAutoKey] != true {
		return
	}
	deferred.ScheduleLastUsedUpdate(a.ID)
}

// Pure, bounded fetch shared by legacy CN quota probing and the new window UI.
// It never writes account scheduling, billing, health, or cooldown state.
func (s *CNProviderQuotaService) fetchOpenCodeGoUsage(ctx context.Context, a *Account, target string) (*OpenCodeGoUsageSnapshot, error) {
	if a == nil || strings.TrimSpace(a.GetCredential("api_key")) == "" {
		return nil, infraerrors.BadRequest("CN_QUOTA_NO_APIKEY", "account api_key is empty")
	}
	validated, err := cnValidateProbeURL(s.cfg, target)
	if err != nil {
		return nil, err
	}
	proxy := s.resolveProxyURL(ctx, a)
	if a.ProxyID != nil && proxy == "" {
		return nil, errors.New("account proxy unavailable")
	}
	headers := http.Header{"Authorization": []string{"Bearer " + strings.TrimSpace(a.GetCredential("api_key"))}, "Accept": []string{"application/json"}}
	a.ApplyHeaderOverrides(headers)
	scopeRaw, _ := json.Marshal([]any{validated, proxy, headers})
	sum := sha256.Sum256(scopeRaw)
	result := s.flight.DoChan("opencode_usage:"+hex.EncodeToString(sum[:]), func() (any, error) {
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if s.usageLock != nil || s.usageDB != nil {
			release, acquired := tryAcquireFixedLeaderLock(callCtx, s.usageLock, s.usageDB, "opencode-go:fetch:"+hex.EncodeToString(sum[:]), uuid.NewString(), 30*time.Second)
			if !acquired {
				return nil, ErrOpenCodeGoUsageBusy
			}
			defer release()
		}
		req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(callCtx), http.MethodGet, validated, nil)
		if err != nil {
			return nil, err
		}
		req.Header = headers.Clone()
		response, err := s.httpUpstream.Do(req, proxy, a.ID, maxInt(a.Concurrency, 1))
		if err != nil {
			return nil, errors.New("OpenCode usage request failed")
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, cnQuotaMaxBodyBytes+1))
		if err != nil {
			return nil, errors.New("OpenCode usage response interrupted")
		}
		if len(body) > cnQuotaMaxBodyBytes {
			return nil, errors.New("OpenCode usage response too large")
		}
		now := time.Now().UTC()
		snapshot := &OpenCodeGoUsageSnapshot{LastAttemptAt: now.Unix(), HTTPStatus: response.StatusCode}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			snapshot.Error = fmt.Sprintf("OpenCode usage returned HTTP %d", response.StatusCode)
			if delay, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && delay > 0 {
				snapshot.NextRefreshAt = now.Add(time.Duration(min(delay, 86400)) * time.Second).Unix()
			} else if at, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
				snapshot.NextRefreshAt = min(at.Unix(), now.Add(24*time.Hour).Unix())
			}
			return snapshot, nil
		}
		snapshot.Tiers = parseOpenCodeGoUsageTiers(body)
		if len(snapshot.Tiers) == 0 {
			snapshot.Error = "OpenCode usage response contains no valid windows"
		} else {
			snapshot.FetchedAt = now.Unix()
		}
		return snapshot, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-result:
		if r.Err != nil {
			return nil, r.Err
		}
		copy := *r.Val.(*OpenCodeGoUsageSnapshot)
		copy.Tiers = append([]CNQuotaTier(nil), copy.Tiers...)
		return &copy, nil
	}
}

type OpenCodeGoUsageService struct {
	accounts AccountRepository
	settings SettingRepository
	quota    *CNProviderQuotaService
	lock     LeaderLockCache
	db       *sql.DB
	mu       sync.Mutex
	config   OpenCodeGoUsageSettings
	cancel   context.CancelFunc
	stopped  bool
	wg       sync.WaitGroup
}

func defaultOpenCodeGoUsageSettings() OpenCodeGoUsageSettings {
	return OpenCodeGoUsageSettings{IntervalMinutes: 15, DebounceMinutes: 1}
}
func (s *OpenCodeGoUsageService) GetSettings(ctx context.Context) (*OpenCodeGoUsageSettings, error) {
	cfg := defaultOpenCodeGoUsageSettings()
	raw, err := s.settings.GetValue(ctx, SettingKeyOpenCodeGoUsageSettings)
	if errors.Is(err, ErrSettingNotFound) || (err == nil && raw == "") {
		return &cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	if err := validateOpenCodeGoUsageSettings(cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
func validateOpenCodeGoUsageSettings(cfg OpenCodeGoUsageSettings) error {
	if cfg.IntervalMinutes < 5 || cfg.IntervalMinutes > 1440 || cfg.DebounceMinutes < 1 || cfg.DebounceMinutes > 60 {
		return infraerrors.BadRequest("OPENCODE_USAGE_SETTINGS_INVALID", "interval must be 5–1440 minutes and debounce 1–60 minutes")
	}
	return nil
}
func (s *OpenCodeGoUsageService) UpdateSettings(ctx context.Context, cfg OpenCodeGoUsageSettings) error {
	if err := validateOpenCodeGoUsageSettings(cfg); err != nil {
		return err
	}
	raw, _ := json.Marshal(cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.settings.Set(ctx, SettingKeyOpenCodeGoUsageSettings, string(raw)); err != nil {
		return err
	}
	s.config = cfg
	openCodeGoUsageAutoEnabled.Store(cfg.Enabled)
	if !cfg.Enabled && s.cancel != nil {
		s.cancel()
	}
	return nil
}
func (s *OpenCodeGoUsageService) state(a *Account) *OpenCodeGoUsageState {
	return &OpenCodeGoUsageState{AccountID: a.ID, Eligible: IsOpenCodeGoUsageAccount(a), AutoRefresh: a.Extra[OpenCodeGoUsageAutoKey] == true, GlobalEnabled: openCodeGoUsageAutoEnabled.Load(), Source: OpenCodeGoUsageSource(a), Snapshot: OpenCodeGoUsageSnapshotForAccount(a)}
}
func (s *OpenCodeGoUsageService) GetState(ctx context.Context, id int64) (*OpenCodeGoUsageState, error) {
	a, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.state(a), nil
}
func (s *OpenCodeGoUsageService) Configure(ctx context.Context, id int64, enabled bool, source string) (*OpenCodeGoUsageState, error) {
	a, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !IsOpenCodeGoUsageAccount(a) {
		return nil, infraerrors.BadRequest("OPENCODE_USAGE_INELIGIBLE", "account is not eligible")
	}
	if source != "configured" && source != "official" {
		return nil, infraerrors.BadRequest("OPENCODE_USAGE_SOURCE_INVALID", "unknown usage source")
	}
	if source == "official" && a.Platform != PlatformOpenCodeGo {
		return nil, infraerrors.BadRequest("OPENCODE_USAGE_SOURCE_INVALID", "official usage is only available for native OpenCode Go accounts")
	}
	repo, ok := s.accounts.(openCodeGoUsageRepository)
	if !ok {
		return nil, errors.New("usage repository unavailable")
	}
	saved, err := repo.ConfigureOpenCodeGoUsage(ctx, a, enabled, source)
	if err != nil {
		return nil, err
	}
	if !saved {
		return nil, ErrOpenCodeGoUsageBusy
	}
	return s.GetState(ctx, id)
}
func (s *OpenCodeGoUsageService) Refresh(ctx context.Context, id int64) (*OpenCodeGoUsageState, error) {
	a, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.refreshAccount(ctx, a, true); err != nil {
		return nil, err
	}
	return s.GetState(ctx, id)
}
func (s *OpenCodeGoUsageService) refreshAccount(ctx context.Context, a *Account, manual bool) error {
	target, err := openCodeGoUsageURL(a)
	if err != nil {
		return err
	}
	if strings.TrimSpace(a.GetCredential("api_key")) == "" {
		return errors.New("account API key is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	repo, ok := s.accounts.(openCodeGoUsageRepository)
	if !ok {
		return errors.New("usage repository unavailable")
	}
	proxy := s.quota.resolveProxyURL(ctx, a)
	if a.ProxyID != nil && (proxy == "" || a.Proxy == nil) {
		return errors.New("account proxy unavailable")
	}
	headers := http.Header{}
	a.ApplyHeaderOverrides(headers)
	var proxyVersion any
	if a.Proxy != nil {
		proxyVersion = a.Proxy.UpdatedAt
	}
	raw, _ := json.Marshal([]any{target, a.GetCredential("api_key"), a.ProxyID, proxy, proxyVersion, headers})
	sum := sha256.Sum256(raw)
	scope := hex.EncodeToString(sum[:])
	release, acquired := tryAcquireFixedLeaderLock(ctx, s.lock, s.db, "opencode-go:usage:"+scope, uuid.NewString(), 30*time.Second)
	if !acquired {
		return ErrOpenCodeGoUsageBusy
	}
	defer release()
	previous := OpenCodeGoUsageSnapshotForAccount(a)
	now := time.Now().UTC()
	if previous != nil && (now.Unix() < previous.LastAttemptAt+30 || now.Unix() < previous.NextRefreshAt) {
		return ErrOpenCodeGoUsageBusy
	}
	cached, err := repo.FindRecentOpenCodeGoUsage(ctx, scope, now.Add(-30*time.Second).Unix())
	if err != nil {
		return err
	}
	var snapshot *OpenCodeGoUsageSnapshot
	if cached != nil {
		snapshot = cached
	} else {
		snapshot, err = s.quota.fetchOpenCodeGoUsage(ctx, a, target)
		if err != nil {
			snapshot = &OpenCodeGoUsageSnapshot{LastAttemptAt: now.Unix(), Error: "OpenCode usage request failed; please retry"}
		}
		if snapshot.Error != "" {
			snapshot.FailureCount = 1
			if previous != nil {
				snapshot.FailureCount = min(previous.FailureCount+1, 10)
				snapshot.Tiers = previous.Tiers
				snapshot.FetchedAt = previous.FetchedAt
			}
			backoff := time.Minute * time.Duration(1<<min(snapshot.FailureCount-1, 6))
			if retryAt := now.Add(backoff).Unix(); retryAt > snapshot.NextRefreshAt {
				snapshot.NextRefreshAt = retryAt
			}
		} else {
			snapshot.NextRefreshAt = now.Add(30 * time.Second).Unix()
		}
	}
	if !manual && !openCodeGoUsageAutoEnabled.Load() {
		return context.Canceled
	}
	saved, err := repo.SaveOpenCodeGoUsageSnapshot(ctx, a, scope, snapshot)
	if err != nil {
		return err
	}
	if !saved {
		return ErrOpenCodeGoUsageBusy
	}
	return nil
}

// Tick reuses the existing once-per-minute control-plane refresh. Disabled
// instances create no worker or timer and perform no remote usage requests.
func (s *OpenCodeGoUsageService) Tick(ctx context.Context) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.GetSettings(ctx)
	if err != nil {
		openCodeGoUsageAutoEnabled.Store(false)
		if s.cancel != nil {
			s.cancel()
		}
		slog.Warn("opencode_usage_settings_failed", "error", err)
		return
	}
	s.config = *cfg
	openCodeGoUsageAutoEnabled.Store(cfg.Enabled)
	if !cfg.Enabled || s.stopped {
		if s.cancel != nil {
			s.cancel()
		}
		return
	}
	if s.cancel != nil {
		return
	}
	workerCtx, workerCancel := context.WithTimeout(context.Background(), 90*time.Second)
	s.cancel = workerCancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer workerCancel()
		defer func() { s.mu.Lock(); s.cancel = nil; s.mu.Unlock() }()
		s.runCycle(workerCtx, *cfg)
	}()
}
func (s *OpenCodeGoUsageService) runCycle(ctx context.Context, cfg OpenCodeGoUsageSettings) {
	release, ok := tryAcquireFixedLeaderLock(ctx, s.lock, s.db, "opencode-go:usage-cycle", uuid.NewString(), 2*time.Minute)
	if !ok {
		return
	}
	defer release()
	repo, ok := s.accounts.(openCodeGoUsageRepository)
	if !ok {
		return
	}
	accounts, err := repo.ListDueOpenCodeGoUsageAccounts(ctx, time.Now(), time.Duration(cfg.DebounceMinutes)*time.Minute, time.Duration(cfg.IntervalMinutes)*time.Minute, 20)
	if err != nil {
		slog.Warn("opencode_usage_due_failed", "error", err)
		return
	}
	for start := 0; start < len(accounts) && ctx.Err() == nil; start += 4 {
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 20*time.Second {
			return
		}
		var workers sync.WaitGroup
		for _, a := range accounts[start:min(start+4, len(accounts))] {
			workers.Add(1)
			go func(a *Account) {
				defer workers.Done()
				if IsOpenCodeGoUsageAccount(a) && a.Extra[OpenCodeGoUsageAutoKey] == true {
					if err := s.refreshAccount(ctx, a, false); err != nil && !errors.Is(err, ErrOpenCodeGoUsageBusy) {
						slog.Debug("opencode_usage_refresh_skipped", "account_id", a.ID, "error", err)
					}
				}
			}(a)
		}
		workers.Wait()
	}
}
func (s *OpenCodeGoUsageService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopped = true
	openCodeGoUsageAutoEnabled.Store(false)
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func stripOpenCodeGoUsageManagedKeys(extra map[string]any) {
	for key := range extra {
		if IsOpenCodeGoUsageManagedKey(key) {
			delete(extra, key)
		}
	}
}

// Export/import must not expose internal subscription fingerprints or import snapshots.
func RedactOpenCodeGoUsageExtra(extra map[string]any) map[string]any {
	out := make(map[string]any, len(extra))
	for key, value := range extra {
		if key != OpenCodeGoUsageScopeKey && key != OpenCodeGoUsageIdentityKey {
			out[key] = value
		}
	}
	return out
}
