package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	OllamaCloudUsageSessionExtraKey     = "ollama_cloud_usage_session"
	OllamaCloudUsageAutoRefreshExtraKey = "ollama_cloud_usage_auto_refresh"
	OllamaCloudUsageSnapshotExtraKey    = "ollama_cloud_usage_snapshot"
	SettingKeyOllamaCloudUsageSettings  = "ollama_cloud_usage_settings"
	ollamaCloudSettingsURL              = "https://ollama.com/settings"
	ollamaCloudMaxBodyBytes             = 512 * 1024
	ollamaCloudMaxRefreshEntries        = 4096
)

var (
	ErrOllamaCloudUsageUnavailable        = infraerrors.ServiceUnavailable("OLLAMA_CLOUD_USAGE_UNAVAILABLE", "Ollama Cloud usage is unavailable")
	ErrOllamaCloudUsageAccountInvalid     = infraerrors.BadRequest("OLLAMA_CLOUD_USAGE_ACCOUNT_INVALID", "account must be an OpenAI or Anthropic API key using ollama.com")
	ErrOllamaCloudUsageSessionRequired    = infraerrors.BadRequest("OLLAMA_CLOUD_USAGE_SESSION_REQUIRED", "an Ollama web session must be configured first")
	ErrOllamaCloudUsageEncryptionKey      = infraerrors.BadRequest("OLLAMA_CLOUD_USAGE_ENCRYPTION_KEY_NOT_CONFIGURED", "a fixed TOTP_ENCRYPTION_KEY is required")
	ErrOllamaCloudUsageRefreshRateLimited = infraerrors.TooManyRequests("OLLAMA_CLOUD_USAGE_REFRESH_RATE_LIMITED", "manual Ollama Cloud refresh is limited to once every 30 seconds")
)

type OllamaCloudUsageSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}
type OllamaCloudUsageWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetText   string  `json:"reset_text,omitempty"`
}
type OllamaCloudUsageModel struct {
	Model    string `json:"model"`
	Window   string `json:"window"`
	Requests int64  `json:"requests"`
}
type OllamaCloudUsageData struct {
	Plan     string                  `json:"plan,omitempty"`
	FiveHour *OllamaCloudUsageWindow `json:"five_hour,omitempty"`
	SevenDay *OllamaCloudUsageWindow `json:"seven_day,omitempty"`
	Models   []OllamaCloudUsageModel `json:"models,omitempty"`
}
type OllamaCloudUsageSnapshot struct {
	Status        string                `json:"status"`
	Data          *OllamaCloudUsageData `json:"data,omitempty"`
	FetchedAt     *time.Time            `json:"fetched_at,omitempty"`
	LastAttemptAt time.Time             `json:"last_attempt_at"`
	NextRefreshAt time.Time             `json:"next_refresh_at"`
	FailureCount  int                   `json:"failure_count,omitempty"`
	HTTPStatus    int                   `json:"http_status,omitempty"`
	LastError     string                `json:"last_error,omitempty"`
}
type OllamaCloudUsageState struct {
	AccountID               int64                     `json:"account_id"`
	Eligible                bool                      `json:"eligible"`
	Configured              bool                      `json:"configured"`
	AutoRefreshEnabled      bool                      `json:"auto_refresh_enabled"`
	EncryptionKeyConfigured bool                      `json:"encryption_key_configured"`
	Snapshot                *OllamaCloudUsageSnapshot `json:"snapshot,omitempty"`
}

type ollamaCloudUsageRepository interface {
	UpdateExtra(context.Context, int64, map[string]any) error
	BulkUpdate(context.Context, []int64, AccountBulkUpdate) (int64, error)
}

// IsOllamaCloudUsageAccount is intentionally strict: it never changes gateway routing eligibility.
func IsOllamaCloudUsageAccount(a *Account) bool {
	if a == nil || a.Type != AccountTypeAPIKey || (a.Platform != PlatformOpenAI && a.Platform != PlatformAnthropic) {
		return false
	}
	base, _ := a.Credentials["base_url"].(string)
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	return strings.EqualFold(base, "https://ollama.com") || strings.EqualFold(base, "https://ollama.com/v1")
}

type OllamaCloudUsageService struct {
	accountRepo             AccountRepository
	httpUpstream            HTTPUpstream
	settingService          *SettingService
	encryptor               SecretEncryptor
	encryptionKeyConfigured bool
	ctx                     context.Context
	cancel                  context.CancelFunc
	wg                      sync.WaitGroup
	refreshMu               sync.Mutex
	refreshAt               map[int64]time.Time
	stopped                 bool
}

func NewOllamaCloudUsageService(repo AccountRepository, upstream HTTPUpstream, settings *SettingService, encryptor SecretEncryptor, keyConfigured bool) *OllamaCloudUsageService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &OllamaCloudUsageService{accountRepo: repo, httpUpstream: upstream, settingService: settings, encryptor: encryptor, encryptionKeyConfigured: keyConfigured, ctx: ctx, cancel: cancel, refreshAt: make(map[int64]time.Time)}
	s.wg.Add(1)
	go s.run()
	return s
}

func ProvideOllamaCloudUsageService(repo AccountRepository, upstream HTTPUpstream, settings *SettingService, encryptor SecretEncryptor, cfg *config.Config) *OllamaCloudUsageService {
	configured := cfg != nil && cfg.Totp.EncryptionKeyConfigured
	return NewOllamaCloudUsageService(repo, upstream, settings, encryptor, configured)
}

func (s *OllamaCloudUsageService) Stop() {
	if s == nil {
		return
	}
	s.refreshMu.Lock()
	if !s.stopped {
		s.stopped = true
		s.cancel()
	}
	s.refreshMu.Unlock()
	s.wg.Wait()
}
func (s *OllamaCloudUsageService) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			_ = s.RunDue(s.ctx)
		}
	}
}

func (s *OllamaCloudUsageService) GetSettings(ctx context.Context) (*OllamaCloudUsageSettings, error) {
	defaults := &OllamaCloudUsageSettings{IntervalMinutes: 60}
	if s == nil || s.settingService == nil {
		return defaults, nil
	}
	raw, err := s.settingService.settingRepo.GetValue(ctx, SettingKeyOllamaCloudUsageSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaults, nil
		}
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return defaults, nil
	}
	if err := json.Unmarshal([]byte(raw), defaults); err != nil {
		return nil, err
	}
	if defaults.IntervalMinutes < 15 {
		defaults.IntervalMinutes = 15
	}
	if defaults.IntervalMinutes > 1440 {
		defaults.IntervalMinutes = 1440
	}
	return defaults, nil
}
func (s *OllamaCloudUsageService) UpdateSettings(ctx context.Context, v *OllamaCloudUsageSettings) error {
	if s == nil || s.settingService == nil || v == nil {
		return ErrOllamaCloudUsageUnavailable
	}
	if v.IntervalMinutes < 15 || v.IntervalMinutes > 1440 {
		return infraerrors.BadRequest("INVALID_OLLAMA_CLOUD_USAGE_INTERVAL", "interval_minutes must be between 15 and 1440")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.settingService.settingRepo.Set(ctx, SettingKeyOllamaCloudUsageSettings, string(b))
}

func (s *OllamaCloudUsageService) GetState(ctx context.Context, id int64) (*OllamaCloudUsageState, error) {
	a, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return ollamaCloudState(a, s.encryptionKeyConfigured), nil
}

func (s *OllamaCloudUsageService) SaveSession(ctx context.Context, id int64, raw string) (*OllamaCloudUsageState, error) {
	if !s.encryptionKeyConfigured || s.encryptor == nil {
		return nil, ErrOllamaCloudUsageEncryptionKey
	}
	a, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !IsOllamaCloudUsageAccount(a) {
		return nil, ErrOllamaCloudUsageAccountInvalid
	}
	cookie, err := normalizeOllamaCloudCookie(raw)
	if err != nil {
		return nil, infraerrors.BadRequest("INVALID_OLLAMA_CLOUD_USAGE_SESSION", err.Error())
	}
	ciphertext, err := s.encryptor.Encrypt(cookie)
	if err != nil {
		return nil, err
	}
	if err = s.accountRepo.UpdateExtra(ctx, id, map[string]any{OllamaCloudUsageSessionExtraKey: ciphertext, OllamaCloudUsageAutoRefreshExtraKey: true}); err != nil {
		return nil, err
	}
	return s.GetState(ctx, id)
}
func (s *OllamaCloudUsageService) DeleteSession(ctx context.Context, id int64) (*OllamaCloudUsageState, error) {
	a, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !IsOllamaCloudUsageAccount(a) {
		return nil, ErrOllamaCloudUsageAccountInvalid
	}
	_, err = s.accountRepo.BulkUpdate(ctx, []int64{id}, AccountBulkUpdate{ExtraRemoveKeys: []string{OllamaCloudUsageSessionExtraKey, OllamaCloudUsageAutoRefreshExtraKey, OllamaCloudUsageSnapshotExtraKey}})
	if err != nil {
		return nil, err
	}
	return s.GetState(ctx, id)
}
func (s *OllamaCloudUsageService) SetAutoRefresh(ctx context.Context, id int64, enabled bool) (*OllamaCloudUsageState, error) {
	a, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if enabled && !ollamaCloudConfigured(a) {
		return nil, ErrOllamaCloudUsageSessionRequired
	}
	if err = s.accountRepo.UpdateExtra(ctx, id, map[string]any{OllamaCloudUsageAutoRefreshExtraKey: enabled}); err != nil {
		return nil, err
	}
	return s.GetState(ctx, id)
}

func (s *OllamaCloudUsageService) Refresh(ctx context.Context, id int64) (*OllamaCloudUsageState, error) {
	s.refreshMu.Lock()
	if id <= 0 {
		s.refreshMu.Unlock()
		return nil, ErrOllamaCloudUsageAccountInvalid
	}
	if at := s.refreshAt[id]; !at.IsZero() && time.Since(at) < 30*time.Second {
		s.refreshMu.Unlock()
		return nil, ErrOllamaCloudUsageRefreshRateLimited
	}
	if len(s.refreshAt) >= ollamaCloudMaxRefreshEntries {
		var oldestID int64
		var oldest time.Time
		for candidateID, candidateAt := range s.refreshAt {
			if oldestID == 0 || candidateAt.Before(oldest) {
				oldestID = candidateID
				oldest = candidateAt
			}
		}
		if oldestID != 0 {
			delete(s.refreshAt, oldestID)
		}
	}
	s.refreshAt[id] = time.Now()
	s.refreshMu.Unlock()
	if _, err := s.refreshAccount(ctx, id, 60); err != nil {
		return nil, err
	}
	return s.GetState(ctx, id)
}

func (s *OllamaCloudUsageService) RunDue(ctx context.Context) error {
	settings, err := s.GetSettings(ctx)
	if err != nil || !settings.Enabled {
		return err
	}
	seen := make(map[int64]struct{})
	jobs := make([]Account, 0, 20)
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		accounts, e := s.accountRepo.ListByPlatform(ctx, platform)
		if e != nil {
			return e
		}
		for i := range accounts {
			a := accounts[i]
			if len(jobs) >= 20 || !a.IsActive() || !IsOllamaCloudUsageAccount(&a) || !ollamaCloudAutoRefresh(&a) || !ollamaCloudDue(&a) {
				continue
			}
			if _, ok := seen[a.ID]; ok {
				continue
			}
			seen[a.ID] = struct{}{}
			jobs = append(jobs, a)
		}
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range jobs {
		a := jobs[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			_, _ = s.refreshAccount(ctx, a.ID, settings.IntervalMinutes)
		}()
	}
	wg.Wait()
	return nil
}

func (s *OllamaCloudUsageService) refreshAccount(ctx context.Context, id int64, interval int) (*OllamaCloudUsageSnapshot, error) {
	a, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !IsOllamaCloudUsageAccount(a) {
		return nil, ErrOllamaCloudUsageAccountInvalid
	}
	enc, _ := a.Extra[OllamaCloudUsageSessionExtraKey].(string)
	if enc == "" {
		return nil, ErrOllamaCloudUsageSessionRequired
	}
	cookie, err := s.encryptor.Decrypt(enc)
	if err != nil {
		return nil, ErrOllamaCloudUsageUnavailable
	}
	cookie, err = normalizeOllamaCloudCookie(cookie)
	if err != nil {
		return nil, ErrOllamaCloudUsageUnavailable
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, ollamaCloudSettingsURL, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "sub2api-ollama-usage/1")
	proxy := ""
	if a.Proxy != nil {
		proxy = a.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxy, a.ID, a.Concurrency)
	now := time.Now().UTC()
	snap := &OllamaCloudUsageSnapshot{LastAttemptAt: now, NextRefreshAt: now.Add(time.Duration(interval) * time.Minute)}
	if err != nil || resp == nil {
		if err == nil {
			err = ErrOllamaCloudUsageUnavailable
		}
		snap.Status = "failed"
		snap.LastError = "request_failed"
		_ = s.accountRepo.UpdateExtra(ctx, id, map[string]any{OllamaCloudUsageSnapshotExtraKey: snap})
		return snap, err
	}
	defer resp.Body.Close()
	snap.HTTPStatus = resp.StatusCode
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, ollamaCloudMaxBodyBytes+1))
	if readErr != nil || len(body) > ollamaCloudMaxBodyBytes {
		snap.Status = "failed"
		snap.LastError = "response_too_large"
	} else if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		snap.Status = "unauthorized"
		snap.LastError = "unauthorized"
	} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snap.Status = "failed"
		snap.LastError = "http_error"
	} else {
		data := parseOllamaCloudUsageHTML(string(body))
		snap.Status = "ok"
		snap.Data = data
		t := now
		snap.FetchedAt = &t
	}
	_ = s.accountRepo.UpdateExtra(ctx, id, map[string]any{OllamaCloudUsageSnapshotExtraKey: snap})
	if snap.Status != "ok" {
		return snap, ErrOllamaCloudUsageUnavailable
	}
	return snap, nil
}

func ollamaCloudState(a *Account, keyConfigured bool) *OllamaCloudUsageState {
	st := &OllamaCloudUsageState{EncryptionKeyConfigured: keyConfigured}
	if a == nil {
		return st
	}
	st.AccountID = a.ID
	st.Eligible = IsOllamaCloudUsageAccount(a)
	st.Configured = ollamaCloudConfigured(a)
	st.AutoRefreshEnabled = ollamaCloudAutoRefresh(a)
	if raw, ok := a.Extra[OllamaCloudUsageSnapshotExtraKey]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &st.Snapshot)
	}
	return st
}
func ollamaCloudConfigured(a *Account) bool {
	if a == nil || a.Extra == nil {
		return false
	}
	v, ok := a.Extra[OllamaCloudUsageSessionExtraKey].(string)
	return ok && v != ""
}
func ollamaCloudAutoRefresh(a *Account) bool {
	if a == nil || a.Extra == nil {
		return false
	}
	v, _ := a.Extra[OllamaCloudUsageAutoRefreshExtraKey].(bool)
	return v
}
func ollamaCloudDue(a *Account) bool {
	if a == nil || !ollamaCloudConfigured(a) || !ollamaCloudAutoRefresh(a) {
		return false
	}
	var snap OllamaCloudUsageSnapshot
	b, _ := json.Marshal(a.Extra[OllamaCloudUsageSnapshotExtraKey])
	if json.Unmarshal(b, &snap) != nil || snap.NextRefreshAt.IsZero() {
		return true
	}
	return !time.Now().Before(snap.NextRefreshAt)
}

var ollamaCookiePair = regexp.MustCompile(`^[^=;\s]+=[^;\r\n]+(?:;\s*[^=;\s]+=[^;\r\n]+)*$`)

func normalizeOllamaCloudCookie(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 16*1024 || strings.ContainsAny(raw, "\r\n") || !ollamaCookiePair.MatchString(raw) {
		return "", fmt.Errorf("session must be a Cookie header containing name=value pairs")
	}
	if strings.Contains(strings.ToLower(raw), "expires=") || strings.Contains(strings.ToLower(raw), "path=") {
		return "", fmt.Errorf("paste a Cookie header, not Set-Cookie")
	}
	return raw, nil
}

// Match the first percentage after the usage label. The lazy prefix is
// important for decimal values such as 12.5%; a greedy prefix would backtrack
// to the final digit and report 5 instead.
var ollamaPercentRE = regexp.MustCompile(`(?i)(?:usage|used|utilization)[^%]{0,80}?(\d{1,3}(?:\.\d+)?)%`)

func parseOllamaCloudUsageHTML(raw string) *OllamaCloudUsageData {
	d := &OllamaCloudUsageData{}
	lower := strings.ToLower(raw)
	if i := strings.Index(lower, "plan"); i >= 0 {
		d.Plan = strings.TrimSpace(strings.Split(strings.TrimSpace(raw[i+4:]), "<")[0])
	}
	matches := ollamaPercentRE.FindAllStringSubmatch(raw, -1)
	for _, m := range matches {
		p, _ := strconv.ParseFloat(m[1], 64)
		if d.FiveHour == nil {
			d.FiveHour = &OllamaCloudUsageWindow{UsedPercent: p}
		} else if d.SevenDay == nil {
			d.SevenDay = &OllamaCloudUsageWindow{UsedPercent: p}
		}
	}
	return d
}
