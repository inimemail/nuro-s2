package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"golang.org/x/sync/singleflight"
)

const (
	grokQuotaUpstreamTimeout = 20 * time.Second
	grokQuotaProbeInput      = "."
	grokQuotaDefaultModel    = grokDefaultResponsesModel
	grokBillingExtraKey      = "grok_billing_snapshot"
)

type GrokQuotaProbeResult struct {
	Source          string              `json:"source"`
	Model           string              `json:"model,omitempty"`
	Billing         *xai.BillingSummary `json:"billing,omitempty"`
	Snapshot        *xai.QuotaSnapshot  `json:"snapshot,omitempty"`
	StatusCode      int                 `json:"status_code,omitempty"`
	HeadersObserved bool                `json:"headers_observed"`
	ResetSupported  bool                `json:"reset_supported"`
	FetchedAt       int64               `json:"fetched_at"`
	Persisted       bool                `json:"persisted"`
	ProbeError      string              `json:"probe_error,omitempty"`
}

type GrokQuotaResetResult struct {
	Supported bool   `json:"supported"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type GrokQuotaService struct {
	accountRepo   AccountRepository
	proxyRepo     ProxyRepository
	tokenProvider *GrokTokenProvider
	httpUpstream  HTTPUpstream
	cfg           *config.Config
	runtimeState  grokRateLimitRuntimeRecovery
	probeFlight   singleflight.Group
}

type grokRateLimitRuntimeRecovery interface {
	clearGrokRateLimitRuntimeBlockAfterRecovery(account *Account)
}

func NewGrokQuotaService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	tokenProvider *GrokTokenProvider,
	httpUpstream HTTPUpstream,
) *GrokQuotaService {
	return &GrokQuotaService{
		accountRepo:   accountRepo,
		proxyRepo:     proxyRepo,
		tokenProvider: tokenProvider,
		httpUpstream:  httpUpstream,
	}
}

func (s *GrokQuotaService) SetRuntimeRecovery(runtimeState grokRateLimitRuntimeRecovery) {
	if s != nil {
		s.runtimeState = runtimeState
	}
}

// SetConfig supplies the runtime URL security policy used by quota probes.
// Keeping this as a setter preserves the lightweight constructor used by unit tests.
func (s *GrokQuotaService) SetConfig(cfg *config.Config) {
	if s != nil {
		s.cfg = cfg
	}
}

func (s *GrokQuotaService) ProbeUsage(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	return s.runProbeFlight(ctx, "active:"+strconv.FormatInt(accountID, 10), func(sharedCtx context.Context) (*GrokQuotaProbeResult, error) {
		return s.probeUsage(sharedCtx, accountID)
	})
}

func (s *GrokQuotaService) QueryQuota(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	billingResult, billingErr := s.ProbeBilling(ctx, accountID)
	if billingErr == nil && billingResult != nil && grokBillingHasAuthoritativeQuota(billingResult.Billing) {
		return billingResult, nil
	}
	probeResult, probeErr := s.ProbeUsage(ctx, accountID)
	if probeErr != nil {
		if billingResult != nil && billingResult.Billing != nil {
			billingResult.ProbeError = safeUpstreamErrorMessage
			return billingResult, nil
		}
		if billingErr != nil {
			return nil, billingErr
		}
		return nil, probeErr
	}
	if billingResult != nil {
		probeResult.Source = "hybrid_probe"
		probeResult.Billing = billingResult.Billing
		probeResult.Persisted = probeResult.Persisted || billingResult.Persisted
	}
	return probeResult, nil
}

func grokBillingHasAuthoritativeQuota(billing *xai.BillingSummary) bool {
	return billing != nil && (billing.UsagePercent != nil ||
		billing.UsedPercent != nil ||
		(billing.MonthlyLimitCents != nil && *billing.MonthlyLimitCents > 0) ||
		strings.TrimSpace(billing.Plan) != "")
}

func (s *GrokQuotaService) probeUsage(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	account, token, proxyURL, err := s.prepareProbe(ctx, accountID)
	if err != nil {
		return nil, err
	}

	probeModel := grokQuotaProbeModel()
	body, err := buildGrokQuotaProbeBody(probeModel)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "GROK_QUOTA_PROBE_BODY_ERROR", "failed to build quota probe")
	}
	targetURL, err := buildGrokResponsesURL(account, s.cfg)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "GROK_QUOTA_BASE_URL_INVALID", "invalid quota endpoint configuration")
	}

	callCtx, cancel := context.WithTimeout(ctx, grokQuotaUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "GROK_QUOTA_PROBE_REQUEST_BUILD_FAILED", "failed to build quota probe request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if account.IsGrokOAuth() && isGrokOfficialOAuthTarget(targetURL) {
		applyGrokCLIHeaders(req.Header)
	} else {
		req.Header.Set("User-Agent", grokUpstreamUserAgent)
	}
	account.ApplyHeaderOverrides(req.Header)

	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxGrokProbeConcurrency(account.Concurrency, 1))
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_PROBE_REQUEST_FAILED", "quota probe request failed")
	}
	defer func() { _ = resp.Body.Close() }()

	snapshot := xai.ObserveQuotaHeaders(resp.Header, resp.StatusCode, "active_probe")
	resetAt, limited := grokRateLimitResetAtForAccount(account, snapshot, time.Now())
	if limited {
		normalizeGrokExhaustedWindowResets(snapshot, resetAt, time.Now())
	}
	persistErr := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		grokQuotaSnapshotExtraKey: snapshot,
	})
	if limited {
		persistGrokRateLimit(ctx, s.accountRepo, account, resetAt)
	} else if isSuccessfulGrokRateLimitRecovery(account, snapshot) {
		if clearGrokRateLimitAfterRecovery(ctx, s.accountRepo, account) && s.runtimeState != nil {
			s.runtimeState.clearGrokRateLimitRuntimeBlockAfterRecovery(account)
		}
	}

	result := &GrokQuotaProbeResult{
		Source:          "active_probe",
		Model:           probeModel,
		Snapshot:        snapshot,
		StatusCode:      resp.StatusCode,
		HeadersObserved: snapshot.HeadersObserved,
		ResetSupported:  false,
		FetchedAt:       time.Now().Unix(),
		Persisted:       persistErr == nil,
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return result, nil
	}
	if resp.StatusCode >= 400 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		slog.Warn("grok_quota_probe_failed", "account_id", account.ID, "model", probeModel, "status", resp.StatusCode)
		return nil, infraerrors.Newf(mapUpstreamStatusCode(resp.StatusCode), "GROK_QUOTA_PROBE_UPSTREAM_ERROR", "quota probe failed (HTTP %d)", resp.StatusCode)
	}
	return result, nil
}

func (s *GrokQuotaService) ProbeBilling(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	return s.runProbeFlight(ctx, "billing:"+strconv.FormatInt(accountID, 10), func(sharedCtx context.Context) (*GrokQuotaProbeResult, error) {
		return s.probeBilling(sharedCtx, accountID)
	})
}

func (s *GrokQuotaService) probeBilling(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	account, token, proxyURL, err := s.prepareProbe(ctx, accountID)
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, grokQuotaUpstreamTimeout)
	defer cancel()
	type billingPart struct {
		summary *xai.BillingSummary
		status  int
		err     error
	}
	var weekly, monthly billingPart
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		weekly.summary, weekly.status, weekly.err = s.fetchBilling(probeCtx, account, token, proxyURL, true)
	}()
	go func() {
		defer wg.Done()
		monthly.summary, monthly.status, monthly.err = s.fetchBilling(probeCtx, account, token, proxyURL, false)
	}()
	wg.Wait()

	weeklyOK, monthlyOK := weekly.summary != nil, monthly.summary != nil
	if !weeklyOK && !monthlyOK {
		return nil, mergeGrokBillingProbeErrors(weekly.status, monthly.status, weekly.err, monthly.err)
	}
	statusCode := preferSuccessfulBillingStatus(weekly.status, monthly.status, weeklyOK, monthlyOK)
	previous, _ := grokBillingSnapshotFromExtra(account.Extra)
	billing := xai.MergeBillingProbeResult(previous, weekly.summary, monthly.summary, weeklyOK, monthlyOK)
	billing = xai.StampBillingSummary(billing, statusCode, "billing_probe")
	persistErr := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{grokBillingExtraKey: billing})
	if persistErr != nil {
		slog.Warn("grok_billing_persist_failed", "account_id", account.ID, "error", persistErr)
	}
	return &GrokQuotaProbeResult{
		Source:     "billing_probe",
		Billing:    billing,
		StatusCode: statusCode,
		FetchedAt:  time.Now().Unix(),
		Persisted:  persistErr == nil,
	}, nil
}

func (s *GrokQuotaService) runProbeFlight(
	ctx context.Context,
	key string,
	probe func(context.Context) (*GrokQuotaProbeResult, error),
) (*GrokQuotaProbeResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "GROK_QUOTA_NOT_CONFIGURED", "quota service is not configured")
	}
	resultCh := s.probeFlight.DoChan(key, func() (any, error) {
		sharedCtx, cancel := context.WithTimeout(context.Background(), grokQuotaUpstreamTimeout+5*time.Second)
		defer cancel()
		return probe(sharedCtx)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return nil, flightResult.Err
		}
		result, ok := flightResult.Val.(*GrokQuotaProbeResult)
		if !ok || result == nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "GROK_QUOTA_PROBE_RESULT_INVALID", "invalid quota probe result")
		}
		clone := *result
		return &clone, nil
	}
}

func (s *GrokQuotaService) fetchBilling(
	ctx context.Context,
	account *Account,
	token, proxyURL string,
	weekly bool,
) (*xai.BillingSummary, int, error) {
	targetURL, err := buildGrokBillingURL(account, s.cfg, weekly)
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusBadRequest, "GROK_QUOTA_BASE_URL_INVALID", "invalid billing endpoint configuration")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusInternalServerError, "GROK_QUOTA_PROBE_REQUEST_BUILD_FAILED", "failed to build billing request")
	}
	if isGrokCLIProxyTarget(targetURL) {
		xai.ApplyCLIBillingHeaders(req, token)
	} else {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", grokUpstreamUserAgent)
	}
	account.ApplyHeaderOverrides(req.Header)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxGrokProbeConcurrency(account.Concurrency, 2))
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_PROBE_REQUEST_FAILED", "billing request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, resp.StatusCode, infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_BILLING_READ_ERROR", "failed to read billing response")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, resp.StatusCode, nil
	}
	if resp.StatusCode >= 400 {
		slog.Warn("grok_quota_billing_failed", "account_id", account.ID, "weekly", weekly, "status", resp.StatusCode)
		return nil, resp.StatusCode, infraerrors.Newf(mapUpstreamStatusCode(resp.StatusCode), "GROK_QUOTA_PROBE_UPSTREAM_ERROR", "billing request failed (HTTP %d)", resp.StatusCode)
	}
	payload, err := xai.ParseBillingPayload(bodyBytes)
	if err != nil {
		return nil, resp.StatusCode, infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_BILLING_PARSE_ERROR", "failed to parse billing response")
	}
	return xai.BuildBillingSummary(payload.Config), resp.StatusCode, nil
}

func mergeGrokBillingProbeErrors(weeklyStatus, monthlyStatus int, weeklyErr, monthlyErr error) error {
	if weeklyErr != nil && grokBillingProbeErrorKey(weeklyStatus, weeklyErr) == grokBillingProbeErrorKey(monthlyStatus, monthlyErr) {
		return weeklyErr
	}
	if monthlyErr != nil && weeklyStatus == 0 {
		return monthlyErr
	}
	if weeklyStatus == http.StatusTooManyRequests && monthlyStatus == http.StatusTooManyRequests {
		return infraerrors.New(http.StatusTooManyRequests, "GROK_QUOTA_PROBE_UPSTREAM_ERROR", "billing request rate limited")
	}
	return infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_PROBE_PARTS_FAILED", "billing probes failed")
}

func grokBillingProbeErrorKey(status int, err error) string {
	if err == nil {
		return strconv.Itoa(status) + ":empty"
	}
	return strconv.Itoa(status) + ":" + strconv.Itoa(infraerrors.Code(err)) + ":" + infraerrors.Reason(err)
}

func preferSuccessfulBillingStatus(weeklyStatus, monthlyStatus int, weeklyOK, monthlyOK bool) int {
	if weeklyOK && weeklyStatus >= 200 && weeklyStatus < 300 {
		return weeklyStatus
	}
	if monthlyOK && monthlyStatus >= 200 && monthlyStatus < 300 {
		return monthlyStatus
	}
	if weeklyStatus != 0 {
		return weeklyStatus
	}
	return monthlyStatus
}

func (s *GrokQuotaService) ResetQuota(ctx context.Context, accountID int64) (*GrokQuotaResetResult, error) {
	if _, err := s.loadGrokOAuthAccount(ctx, accountID); err != nil {
		return nil, err
	}
	return nil, infraerrors.New(http.StatusNotImplemented, "GROK_QUOTA_RESET_UNSUPPORTED", "quota reset is not supported for OAuth accounts")
}

func (s *GrokQuotaService) prepareProbe(ctx context.Context, accountID int64) (*Account, string, string, error) {
	if s == nil || s.tokenProvider == nil || s.httpUpstream == nil {
		return nil, "", "", infraerrors.New(http.StatusInternalServerError, "GROK_QUOTA_NOT_CONFIGURED", "grok quota service is not configured")
	}
	account, err := s.loadGrokOAuthAccount(ctx, accountID)
	if err != nil {
		return nil, "", "", err
	}

	token, err := s.tokenProvider.GetAccessTokenForProbe(ctx, account)
	if err != nil {
		return nil, "", "", infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_TOKEN_UNAVAILABLE", "failed to acquire access token")
	}
	if strings.TrimSpace(token) == "" {
		return nil, "", "", infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_TOKEN_UNAVAILABLE", "access token is empty")
	}

	return account, token, s.resolveProxyURL(ctx, account), nil
}

func (s *GrokQuotaService) resolveProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil {
		return ""
	}
	switch {
	case account.Proxy != nil:
		return account.Proxy.URL()
	case s != nil && s.proxyRepo != nil:
		if proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && proxy != nil {
			return proxy.URL()
		}
	}
	return ""
}

func (s *GrokQuotaService) loadGrokOAuthAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "GROK_QUOTA_NOT_CONFIGURED", "grok quota service is not configured")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.New(http.StatusNotFound, "GROK_QUOTA_ACCOUNT_NOT_FOUND", "account not found")
	}
	if account == nil {
		return nil, infraerrors.New(http.StatusNotFound, "GROK_QUOTA_ACCOUNT_NOT_FOUND", "account not found")
	}
	if account.Platform != PlatformGrok {
		return nil, infraerrors.New(http.StatusBadRequest, "GROK_QUOTA_INVALID_PLATFORM", "account is not a Grok account")
	}
	if account.Type != AccountTypeOAuth {
		return nil, infraerrors.New(http.StatusBadRequest, "GROK_QUOTA_INVALID_TYPE", "account is not an OAuth account")
	}
	return account, nil
}

func grokQuotaProbeModel() string {
	return grokQuotaDefaultModel
}

func buildGrokQuotaProbeBody(model string) ([]byte, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = grokQuotaDefaultModel
	}
	return json.Marshal(map[string]any{
		"model":             model,
		"input":             grokQuotaProbeInput,
		"max_output_tokens": 1,
		"store":             false,
	})
}

func maxGrokProbeConcurrency(a, b int) int {
	if a > b {
		return a
	}
	return b
}
