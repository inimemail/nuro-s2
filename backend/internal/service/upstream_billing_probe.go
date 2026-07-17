package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const (
	UpstreamBillingProbeExtraKey        = "upstream_billing_probe"
	UpstreamBillingProbeEnabledExtraKey = "upstream_billing_probe_enabled"
	UpstreamBillingProbeMaxBatchSize    = 20

	upstreamBillingProbeDefaultIntervalSeconds = 5
	upstreamBillingProbeMinIntervalSeconds     = 1
	upstreamBillingProbeMaxIntervalSeconds     = 36000
	upstreamBillingProbeRequestTimeout         = 10 * time.Second
	upstreamBillingProbeMaxBodyBytes           = 64 * 1024
	upstreamBillingProbeConcurrency            = 16
	upstreamBillingProbeMaxBackoff             = 24 * time.Hour
	upstreamBillingProbeMinFailureBackoff      = 5 * time.Second
	upstreamBillingProbeSettingsPollInterval   = 5 * time.Second
	upstreamBillingProbeLeaderLockKey          = "upstream:billing:probe:leader"
)

var (
	ErrUpstreamBillingProbeUnavailable = infraerrors.ServiceUnavailable(
		"UPSTREAM_BILLING_PROBE_UNAVAILABLE", "upstream billing probe is unavailable",
	)
	ErrUpstreamBillingProbeAccountInvalid = infraerrors.BadRequest(
		"UPSTREAM_BILLING_PROBE_ACCOUNT_INVALID", "account is not an OpenAI API key account",
	)
	ErrUpstreamBillingProbeIdentityChanged = infraerrors.Conflict(
		"UPSTREAM_BILLING_PROBE_IDENTITY_CHANGED", "account identity changed during upstream billing probe; retry the probe",
	)
	ErrUpstreamBillingGuardRequiresAutoProbe = infraerrors.BadRequest(
		"UPSTREAM_BILLING_GUARD_REQUIRES_AUTO_PROBE", "enable account automatic billing probe before enabling the rate guard",
	)
	ErrUpstreamBillingGuardRequiresGroupLimit = infraerrors.BadRequest(
		"UPSTREAM_BILLING_GUARD_REQUIRES_GROUP_LIMIT", "upstream billing guard requires at least one bound OpenAI group with a configured limit",
	)
	ErrUpstreamBillingProbeRequiredByGuard = infraerrors.Conflict(
		"UPSTREAM_BILLING_PROBE_REQUIRED_BY_GUARD", "disable the upstream billing guard before disabling automatic probe",
	)
	ErrInvalidUpstreamBillingProbeEnabled = infraerrors.BadRequest(
		"INVALID_UPSTREAM_BILLING_PROBE_ENABLED", "upstream_billing_probe_enabled must be a boolean",
	)
)

type UpstreamBillingProbeSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
}

type upstreamBillingProbeSettingsStorage struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds,omitempty"`
	IntervalMinutes int  `json:"interval_minutes,omitempty"`
}

type UpstreamBillingGuardSettings struct {
	Enabled bool `json:"enabled"`
}

type UpstreamBillingGuardResult struct {
	AccountID int64                         `json:"account_id"`
	Account   *Account                      `json:"account,omitempty"`
	Snapshot  *UpstreamBillingProbeSnapshot `json:"snapshot,omitempty"`
}

type UpstreamBillingProbeSnapshot struct {
	Status        string         `json:"status"`
	Data          map[string]any `json:"data,omitempty"`
	ReceivedAt    *time.Time     `json:"received_at,omitempty"`
	FreshUntil    *time.Time     `json:"fresh_until,omitempty"`
	LastAttemptAt time.Time      `json:"last_attempt_at"`
	NextProbeAt   time.Time      `json:"next_probe_at"`
	FailureCount  int            `json:"failure_count,omitempty"`
	HTTPStatus    int            `json:"http_status,omitempty"`
	LastError     string         `json:"last_error,omitempty"`
}

type UpstreamBillingProbeResult struct {
	AccountID int64                         `json:"account_id"`
	Snapshot  *UpstreamBillingProbeSnapshot `json:"snapshot,omitempty"`
	Error     string                        `json:"error,omitempty"`
}

type upstreamBillingProbeResponse struct {
	Object                  string   `json:"object"`
	SchemaVersion           int      `json:"schema_version"`
	BillingScope            string   `json:"billing_scope"`
	GroupRateMultiplier     *float64 `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64 `json:"user_rate_multiplier"`
	ResolvedRateMultiplier  *float64 `json:"resolved_rate_multiplier"`
	PeakRateEnabled         *bool    `json:"peak_rate_enabled"`
	PeakStart               *string  `json:"peak_start"`
	PeakEnd                 *string  `json:"peak_end"`
	PeakRateMultiplier      *float64 `json:"peak_rate_multiplier"`
	AppliedPeakMultiplier   *float64 `json:"applied_peak_multiplier"`
	EffectiveRateMultiplier *float64 `json:"effective_rate_multiplier"`
	Timezone                *string  `json:"timezone"`
	ObservedAt              string   `json:"observed_at"`
}

type upstreamBillingProbeSnapshotWriter interface {
	UpdateUpstreamBillingProbeSnapshot(context.Context, *Account, *UpstreamBillingProbeSnapshot, *float64) (bool, error)
}

type upstreamBillingProbeEnabledWriter interface {
	UpdateUpstreamBillingProbeEnabled(context.Context, int64, bool) error
}

type upstreamBillingGuardWriter interface {
	UpdateUpstreamBillingGuard(context.Context, int64, bool) (*Account, bool, error)
}

type UpstreamBillingProbeService struct {
	accountRepo        AccountRepository
	accountTestService *AccountTestService
	settingService     *SettingService
	db                 *sql.DB
	parentCtx          context.Context
	parentCancel       context.CancelFunc
	wg                 sync.WaitGroup
	wakeCh             chan struct{}
	startMu            sync.Mutex
	started            bool
	stopped            bool
	cycleMu            sync.Mutex
	probeGroup         singleflight.Group
	probeSlots         chan struct{}
	now                func() time.Time
}

func NewUpstreamBillingProbeService(repo AccountRepository, tests *AccountTestService, settings *SettingService) *UpstreamBillingProbeService {
	ctx, cancel := context.WithCancel(context.Background())
	return &UpstreamBillingProbeService{
		accountRepo: repo, accountTestService: tests, settingService: settings,
		parentCtx: ctx, parentCancel: cancel, wakeCh: make(chan struct{}, 1),
		probeSlots: make(chan struct{}, upstreamBillingProbeConcurrency), now: time.Now,
	}
}

func ProvideUpstreamBillingProbeService(repo AccountRepository, tests *AccountTestService, settings *SettingService, db *sql.DB) *UpstreamBillingProbeService {
	svc := NewUpstreamBillingProbeService(repo, tests, settings)
	svc.db = db
	svc.Start()
	return svc
}

func defaultUpstreamBillingProbeSettings() *UpstreamBillingProbeSettings {
	return &UpstreamBillingProbeSettings{Enabled: false, IntervalSeconds: upstreamBillingProbeDefaultIntervalSeconds}
}

func (s *SettingService) GetUpstreamBillingProbeSettings(ctx context.Context) (*UpstreamBillingProbeSettings, error) {
	defaults := defaultUpstreamBillingProbeSettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyUpstreamBillingProbeSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaults, nil
		}
		return nil, fmt.Errorf("get upstream billing probe settings: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return defaults, nil
	}
	var stored upstreamBillingProbeSettingsStorage
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, fmt.Errorf("parse upstream billing probe settings: %w", err)
	}
	intervalSeconds := stored.IntervalSeconds
	if intervalSeconds == 0 && stored.IntervalMinutes > 0 {
		intervalSeconds = stored.IntervalMinutes * 60
	}
	if intervalSeconds == 0 {
		intervalSeconds = defaults.IntervalSeconds
	}
	if intervalSeconds < upstreamBillingProbeMinIntervalSeconds {
		intervalSeconds = upstreamBillingProbeMinIntervalSeconds
	}
	if intervalSeconds > upstreamBillingProbeMaxIntervalSeconds {
		intervalSeconds = upstreamBillingProbeMaxIntervalSeconds
	}
	return &UpstreamBillingProbeSettings{Enabled: stored.Enabled, IntervalSeconds: intervalSeconds}, nil
}

func (s *SettingService) SetUpstreamBillingProbeSettings(ctx context.Context, settings *UpstreamBillingProbeSettings) error {
	if s == nil || s.settingRepo == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	if settings == nil || settings.IntervalSeconds < upstreamBillingProbeMinIntervalSeconds || settings.IntervalSeconds > upstreamBillingProbeMaxIntervalSeconds {
		return infraerrors.BadRequest("INVALID_UPSTREAM_BILLING_PROBE_INTERVAL", "interval_seconds must be between 1 and 36000")
	}
	raw, err := json.Marshal(upstreamBillingProbeSettingsStorage{Enabled: settings.Enabled, IntervalSeconds: settings.IntervalSeconds})
	if err != nil {
		return err
	}
	return s.settingRepo.Set(ctx, SettingKeyUpstreamBillingProbeSettings, string(raw))
}

func (s *UpstreamBillingProbeService) Start() {
	if s == nil {
		return
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	s.wg.Add(1)
	go s.runLoop()
}

func (s *UpstreamBillingProbeService) Stop() {
	if s == nil {
		return
	}
	s.startMu.Lock()
	if s.stopped {
		s.startMu.Unlock()
		return
	}
	s.stopped = true
	s.parentCancel()
	s.startMu.Unlock()
	s.wg.Wait()
}

func (s *UpstreamBillingProbeService) runLoop() {
	defer s.wg.Done()
	nextProbeAt := time.Time{}
	for {
		settings, settingsErr := s.GetSettings(s.parentCtx)
		if settingsErr == nil && settings != nil && settings.Enabled {
			now := s.currentTime()
			if nextProbeAt.IsZero() || !now.Before(nextProbeAt) {
				_ = s.RunDue(s.parentCtx)
				nextProbeAt = s.currentTime().Add(time.Duration(settings.IntervalSeconds) * time.Second)
			}
		}

		wait := upstreamBillingProbeSettingsPollInterval
		if settingsErr == nil && settings != nil && settings.Enabled && !nextProbeAt.IsZero() {
			until := nextProbeAt.Sub(s.currentTime())
			if until <= 0 {
				continue
			}
			if until < wait {
				wait = until
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-s.parentCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		case <-s.wakeCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			nextProbeAt = time.Time{}
		}
	}
}

func (s *UpstreamBillingProbeService) signalWake() {
	if s == nil || s.wakeCh == nil {
		return
	}
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *UpstreamBillingProbeService) RunDue(ctx context.Context) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()
	settings, err := s.GetSettings(ctx)
	if err != nil || !settings.Enabled {
		return err
	}
	release, acquired := tryAcquireDBAdvisoryLock(ctx, s.db, hashAdvisoryLockID(upstreamBillingProbeLeaderLockKey))
	if !acquired {
		return nil
	}
	defer release()
	accounts, err := s.accountRepo.FindByExtraField(ctx, UpstreamBillingProbeEnabledExtraKey, true)
	if err != nil {
		return err
	}
	now := s.currentTime()
	type dueProbe struct {
		accountID int64
		nextAt    time.Time
	}
	due := make([]dueProbe, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if !isUpstreamBillingProbeAccount(account) || !account.IsActive() {
			continue
		}
		snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
		if snapshot == nil || snapshot.NextProbeAt.IsZero() || !now.Before(snapshot.NextProbeAt) {
			nextAt := time.Time{}
			if snapshot != nil {
				nextAt = snapshot.NextProbeAt
			}
			due = append(due, dueProbe{accountID: account.ID, nextAt: nextAt})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].nextAt.Equal(due[j].nextAt) {
			return due[i].accountID < due[j].accountID
		}
		return due[i].nextAt.Before(due[j].nextAt)
	})
	jobs := make(chan int64)
	var group errgroup.Group
	workerCount := upstreamBillingProbeConcurrency
	if len(due) < workerCount {
		workerCount = len(due)
	}
	for i := 0; i < workerCount; i++ {
		group.Go(func() error {
			for id := range jobs {
				_, _ = s.probeScheduledAccount(ctx, id, settings.IntervalSeconds)
			}
			return nil
		})
	}
	group.Go(func() error {
		defer close(jobs)
		for _, item := range due {
			select {
			case jobs <- item.accountID:
			case <-ctx.Done():
				return nil
			}
		}
		return nil
	})
	return group.Wait()
}

func (s *UpstreamBillingProbeService) GetSettings(ctx context.Context) (*UpstreamBillingProbeSettings, error) {
	if s == nil || s.settingService == nil {
		return defaultUpstreamBillingProbeSettings(), nil
	}
	return s.settingService.GetUpstreamBillingProbeSettings(ctx)
}

func (s *UpstreamBillingProbeService) UpdateSettings(ctx context.Context, settings *UpstreamBillingProbeSettings) error {
	if s == nil || s.settingService == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	if err := s.settingService.SetUpstreamBillingProbeSettings(ctx, settings); err != nil {
		return err
	}
	s.signalWake()
	return nil
}

func (s *UpstreamBillingProbeService) SetAccountEnabled(ctx context.Context, id int64, enabled bool) error {
	if s == nil || s.accountRepo == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if !isUpstreamBillingProbeAccount(account) {
		return ErrUpstreamBillingProbeAccountInvalid
	}
	writer, ok := s.accountRepo.(upstreamBillingProbeEnabledWriter)
	if !ok {
		return ErrUpstreamBillingProbeUnavailable
	}
	if err := writer.UpdateUpstreamBillingProbeEnabled(ctx, id, enabled); err != nil {
		return err
	}
	s.signalWake()
	return nil
}

func (s *UpstreamBillingProbeService) UpdateGuard(ctx context.Context, id int64, settings UpstreamBillingGuardSettings) (*UpstreamBillingGuardResult, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrUpstreamBillingProbeUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !isUpstreamBillingProbeAccount(account) {
		return nil, ErrUpstreamBillingProbeAccountInvalid
	}
	if settings.Enabled && !account.HasUpstreamBillingGuardGroupLimit() {
		return nil, ErrUpstreamBillingGuardRequiresGroupLimit
	}
	autoProbeEnabled, _ := account.Extra[UpstreamBillingProbeEnabledExtraKey].(bool)
	if settings.Enabled && !autoProbeEnabled {
		enabledWriter, ok := s.accountRepo.(upstreamBillingProbeEnabledWriter)
		if !ok {
			return nil, ErrUpstreamBillingProbeUnavailable
		}
		if err := enabledWriter.UpdateUpstreamBillingProbeEnabled(ctx, id, true); err != nil {
			return nil, err
		}
		account, err = s.accountRepo.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
	}
	writer, ok := s.accountRepo.(upstreamBillingGuardWriter)
	if !ok {
		return nil, ErrUpstreamBillingProbeUnavailable
	}
	updated, _, err := writer.UpdateUpstreamBillingGuard(ctx, id, settings.Enabled)
	if err != nil {
		return nil, err
	}
	result := &UpstreamBillingGuardResult{AccountID: id, Account: updated}
	if settings.Enabled && updated.UpstreamBillingGuardObservedMultiplier == nil {
		snapshot, probeErr := s.ProbeAccount(ctx, id)
		if probeErr == nil {
			result.Snapshot = snapshot
			result.Account, _ = s.accountRepo.GetByID(ctx, id)
		}
	}
	return result, nil
}

func (s *UpstreamBillingProbeService) ProbeAccount(ctx context.Context, id int64) (*UpstreamBillingProbeSnapshot, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrUpstreamBillingProbeUnavailable
	}
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	value, err, _ := s.probeGroup.Do(strconv.FormatInt(id, 10), func() (any, error) {
		select {
		case s.probeSlots <- struct{}{}:
			defer func() { <-s.probeSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		account, loadErr := s.accountRepo.GetByID(ctx, id)
		if loadErr != nil {
			return nil, loadErr
		}
		if !isUpstreamBillingProbeAccount(account) {
			return nil, ErrUpstreamBillingProbeAccountInvalid
		}
		return s.probe(ctx, account, settings.IntervalSeconds)
	})
	if err != nil {
		return nil, err
	}
	snapshot, _ := value.(*UpstreamBillingProbeSnapshot)
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) probeScheduledAccount(ctx context.Context, id int64, intervalSeconds int) (*UpstreamBillingProbeSnapshot, error) {
	value, err, _ := s.probeGroup.Do(strconv.FormatInt(id, 10), func() (any, error) {
		select {
		case s.probeSlots <- struct{}{}:
			defer func() { <-s.probeSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		account, loadErr := s.accountRepo.GetByID(ctx, id)
		if loadErr != nil {
			return nil, loadErr
		}
		enabled, _ := account.Extra[UpstreamBillingProbeEnabledExtraKey].(bool)
		if !enabled || !account.IsActive() || !isUpstreamBillingProbeAccount(account) {
			return nil, nil
		}
		if snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra); snapshot != nil && !snapshot.NextProbeAt.IsZero() && s.currentTime().Before(snapshot.NextProbeAt) {
			return nil, nil
		}
		return s.probe(ctx, account, intervalSeconds)
	})
	if err != nil || value == nil {
		return nil, err
	}
	snapshot, _ := value.(*UpstreamBillingProbeSnapshot)
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) ProbeAccounts(ctx context.Context, ids []int64) []UpstreamBillingProbeResult {
	if len(ids) > UpstreamBillingProbeMaxBatchSize {
		ids = ids[:UpstreamBillingProbeMaxBatchSize]
	}
	out := make([]UpstreamBillingProbeResult, len(ids))
	var group errgroup.Group
	for i, id := range ids {
		i, id := i, id
		out[i].AccountID = id
		group.Go(func() error {
			snapshot, err := s.ProbeAccount(ctx, id)
			if err != nil {
				out[i].Error = safeProbeError(err)
			} else {
				out[i].Snapshot = snapshot
			}
			return nil
		})
	}
	_ = group.Wait()
	return out
}

func (s *UpstreamBillingProbeService) probe(ctx context.Context, account *Account, intervalSeconds int) (*UpstreamBillingProbeSnapshot, error) {
	now := s.currentTime().UTC()
	if s.accountTestService == nil || s.accountTestService.httpUpstream == nil {
		return s.persistFailure(ctx, account, intervalSeconds, now, 0, "transport_unavailable")
	}
	apiKey := account.GetOpenAIApiKey()
	if apiKey == "" {
		return s.persistFailure(ctx, account, intervalSeconds, now, 0, "missing_api_key")
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	normalized, err := s.accountTestService.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return s.persistFailure(ctx, account, intervalSeconds, now, 0, "invalid_base_url")
	}
	proxyURL := ""
	if account.ProxyID != nil {
		if account.Proxy == nil || account.Proxy.ID != *account.ProxyID {
			return nil, ErrUpstreamBillingProbeIdentityChanged
		}
		proxyURL = account.Proxy.URL()
	}
	probeCtx, cancel := context.WithTimeout(ctx, upstreamBillingProbeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, buildOpenAIEndpointURL(normalized, "/v1/sub2api/billing"), bytes.NewReader(nil))
	if err != nil {
		return s.persistFailure(ctx, account, intervalSeconds, now, 0, "request_build_failed")
	}
	reqCtx := WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileBillingProbe)
	req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(reqCtx))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	account.ApplyHeaderOverrides(req.Header)
	var tlsProfile *tlsfingerprint.Profile
	if resolver := s.accountTestService.tlsFPProfileService; resolver != nil {
		tlsProfile = resolver.ResolveTLSProfile(account)
	}
	resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		return s.persistFailure(ctx, account, intervalSeconds, now, 0, "request_failed")
	}
	if resp == nil || resp.Body == nil {
		return s.persistFailure(ctx, account, intervalSeconds, now, 0, "empty_response")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBillingProbeMaxBodyBytes+1))
	if err != nil || len(body) > upstreamBillingProbeMaxBodyBytes {
		return s.persistFailure(ctx, account, intervalSeconds, now, resp.StatusCode, "response_read_failed")
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return s.persistFailure(ctx, account, intervalSeconds, now, resp.StatusCode, "unsupported")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.persistFailure(ctx, account, intervalSeconds, now, resp.StatusCode, "http_error")
	}
	data, err := parseUpstreamBillingProbeResponse(body)
	if err != nil {
		return s.persistFailure(ctx, account, intervalSeconds, now, resp.StatusCode, "invalid_response")
	}
	observedMultiplier, ok := billingMultiplierFromProbeData(data)
	if !ok {
		return s.persistFailure(ctx, account, intervalSeconds, now, resp.StatusCode, "invalid_response")
	}
	receivedAt := now
	freshWindow := 3 * time.Duration(intervalSeconds) * time.Second
	if freshWindow < time.Minute {
		freshWindow = time.Minute
	}
	freshUntil := now.Add(freshWindow)
	snapshot := &UpstreamBillingProbeSnapshot{
		Status: "ok", Data: data, ReceivedAt: &receivedAt, FreshUntil: &freshUntil,
		LastAttemptAt: now, NextProbeAt: now.Add(time.Duration(intervalSeconds) * time.Second), HTTPStatus: resp.StatusCode,
	}
	if err := s.updateSnapshot(ctx, account, snapshot, &observedMultiplier); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) persistFailure(ctx context.Context, account *Account, intervalSeconds int, now time.Time, statusCode int, reason string) (*UpstreamBillingProbeSnapshot, error) {
	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	failureCount := 1
	if previous != nil {
		failureCount = previous.FailureCount + 1
	}
	status := "failed"
	if reason == "unsupported" {
		status = "unsupported"
	}
	snapshot := &UpstreamBillingProbeSnapshot{
		Status: status, LastAttemptAt: now, NextProbeAt: now.Add(nextUpstreamBillingProbeDelay(intervalSeconds, failureCount)),
		FailureCount: failureCount, HTTPStatus: statusCode, LastError: reason,
	}
	if previous != nil {
		snapshot.Data, snapshot.ReceivedAt, snapshot.FreshUntil = previous.Data, previous.ReceivedAt, previous.FreshUntil
	}
	if err := s.updateSnapshot(ctx, account, snapshot, nil); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) updateSnapshot(ctx context.Context, account *Account, snapshot *UpstreamBillingProbeSnapshot, observedMultiplier *float64) error {
	writer, ok := s.accountRepo.(upstreamBillingProbeSnapshotWriter)
	if !ok {
		return ErrUpstreamBillingProbeUnavailable
	}
	_, err := writer.UpdateUpstreamBillingProbeSnapshot(ctx, account, snapshot, observedMultiplier)
	return err
}

func billingMultiplierFromProbeData(data map[string]any) (float64, bool) {
	if data == nil {
		return 0, false
	}
	value, ok := data["effective_rate_multiplier"].(float64)
	return value, ok && value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func parseUpstreamBillingProbeResponse(body []byte) (map[string]any, error) {
	var response upstreamBillingProbeResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Object != "sub2api.key_billing" || response.SchemaVersion != 1 || response.BillingScope != "token" ||
		response.GroupRateMultiplier == nil || response.ResolvedRateMultiplier == nil || response.PeakRateEnabled == nil || response.EffectiveRateMultiplier == nil {
		return nil, errors.New("unexpected billing response schema")
	}
	for _, value := range []*float64{response.GroupRateMultiplier, response.ResolvedRateMultiplier, response.UserRateMultiplier, response.EffectiveRateMultiplier} {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return nil, errors.New("invalid billing multiplier")
		}
	}
	expectedResolved := *response.GroupRateMultiplier
	if response.UserRateMultiplier != nil {
		expectedResolved = *response.UserRateMultiplier
	}
	if !equalBillingMultiplier(*response.ResolvedRateMultiplier, expectedResolved) {
		return nil, errors.New("inconsistent resolved billing multiplier")
	}
	observedAt, err := time.Parse(time.RFC3339Nano, response.ObservedAt)
	if err != nil || observedAt.IsZero() {
		return nil, errors.New("invalid observed_at")
	}
	data := map[string]any{
		"object": response.Object, "schema_version": response.SchemaVersion, "billing_scope": response.BillingScope,
		"group_rate_multiplier": *response.GroupRateMultiplier, "resolved_rate_multiplier": *response.ResolvedRateMultiplier,
		"peak_rate_enabled": *response.PeakRateEnabled, "effective_rate_multiplier": *response.EffectiveRateMultiplier,
		"observed_at": observedAt.UTC().Format(time.RFC3339Nano),
	}
	if response.UserRateMultiplier != nil {
		data["user_rate_multiplier"] = *response.UserRateMultiplier
	}
	appliedPeak := 1.0
	if *response.PeakRateEnabled {
		if response.PeakStart == nil || response.PeakEnd == nil || response.PeakRateMultiplier == nil || response.AppliedPeakMultiplier == nil || response.Timezone == nil ||
			strings.TrimSpace(*response.PeakStart) == "" || strings.TrimSpace(*response.PeakEnd) == "" || strings.TrimSpace(*response.Timezone) == "" ||
			*response.PeakRateMultiplier < 0 || *response.AppliedPeakMultiplier < 0 ||
			math.IsNaN(*response.PeakRateMultiplier) || math.IsInf(*response.PeakRateMultiplier, 0) ||
			math.IsNaN(*response.AppliedPeakMultiplier) || math.IsInf(*response.AppliedPeakMultiplier, 0) {
			return nil, errors.New("incomplete peak billing response")
		}
		appliedPeak = *response.AppliedPeakMultiplier
		startMinute, startOK := parseMinutes(*response.PeakStart)
		endMinute, endOK := parseMinutes(*response.PeakEnd)
		location, locationErr := time.LoadLocation(*response.Timezone)
		if !startOK || !endOK || startMinute >= endMinute || locationErr != nil {
			return nil, errors.New("invalid peak billing response")
		}
		local := observedAt.In(location)
		expectedPeak := 1.0
		minute := local.Hour()*60 + local.Minute()
		if minute >= startMinute && minute < endMinute {
			expectedPeak = *response.PeakRateMultiplier
		}
		if !equalBillingMultiplier(appliedPeak, expectedPeak) {
			return nil, errors.New("inconsistent applied peak multiplier")
		}
		data["peak_start"], data["peak_end"], data["peak_rate_multiplier"] = *response.PeakStart, *response.PeakEnd, *response.PeakRateMultiplier
		data["applied_peak_multiplier"], data["timezone"] = appliedPeak, *response.Timezone
	}
	if !equalBillingMultiplier(*response.EffectiveRateMultiplier, *response.ResolvedRateMultiplier*appliedPeak) {
		return nil, errors.New("inconsistent effective billing multiplier")
	}
	return data, nil
}

func equalBillingMultiplier(left, right float64) bool {
	scale := math.Max(1, math.Max(math.Abs(left), math.Abs(right)))
	return !math.IsNaN(left) && !math.IsNaN(right) && !math.IsInf(left, 0) && !math.IsInf(right, 0) && math.Abs(left-right) <= 1e-9*scale
}

func decodeUpstreamBillingProbeSnapshot(extra map[string]any) *UpstreamBillingProbeSnapshot {
	if extra == nil {
		return nil
	}
	raw, err := json.Marshal(extra[UpstreamBillingProbeExtraKey])
	if err != nil || string(raw) == "null" {
		return nil
	}
	var snapshot UpstreamBillingProbeSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Status == "" {
		return nil
	}
	return &snapshot
}

func nextUpstreamBillingProbeDelay(intervalSeconds, failureCount int) time.Duration {
	delay := time.Duration(intervalSeconds) * time.Second
	if delay < upstreamBillingProbeMinFailureBackoff {
		delay = upstreamBillingProbeMinFailureBackoff
	}
	if failureCount > 0 {
		shift := failureCount
		if shift > 5 {
			shift = 5
		}
		delay *= time.Duration(1 << shift)
	}
	if delay > upstreamBillingProbeMaxBackoff {
		return upstreamBillingProbeMaxBackoff
	}
	return delay
}

func isUpstreamBillingProbeAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey
}

func (s *UpstreamBillingProbeService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func safeProbeError(err error) string {
	if errors.Is(err, ErrUpstreamBillingProbeAccountInvalid) {
		return ErrUpstreamBillingProbeAccountInvalid.Error()
	}
	if errors.Is(err, ErrUpstreamBillingProbeUnavailable) {
		return ErrUpstreamBillingProbeUnavailable.Error()
	}
	if errors.Is(err, ErrUpstreamBillingProbeIdentityChanged) {
		return ErrUpstreamBillingProbeIdentityChanged.Error()
	}
	return "probe_failed"
}
