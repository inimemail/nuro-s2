package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type SeedanceService struct {
	repo    SeedanceTaskRepository
	gateway *OpenAIGatewayService
	keys    *APIKeyService
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func NewSeedanceService(repo SeedanceTaskRepository, gateway *OpenAIGatewayService, keys *APIKeyService) *SeedanceService {
	return &SeedanceService{repo: repo, gateway: gateway, keys: keys}
}

func ParseSeedanceRequest(body []byte) (GrokMediaRequestInfo, error) {
	var info GrokMediaRequestInfo
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return info, errors.New("request must be a JSON object")
	}
	model := gjson.GetBytes(body, "model")
	content := gjson.GetBytes(body, "content")
	if model.Type != gjson.String || strings.TrimSpace(model.String()) == "" {
		return info, errors.New("model is required")
	}
	if !content.IsArray() || len(content.Array()) == 0 {
		return info, errors.New("content must be a non-empty array")
	}
	info.Model = strings.TrimSpace(model.String())
	for _, part := range content.Array() {
		if !part.IsObject() {
			return info, errors.New("content items must be objects")
		}
		switch part.Get("type").String() {
		case "text":
			info.Prompt += part.Get("text").String() + "\n"
		case "image_url":
			info.InputImageURLs = append(info.InputImageURLs, part.Get("image_url.url").String())
		}
	}
	return info, nil
}

func buildSeedanceURL(base, id string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("invalid Seedance base URL")
	}
	if !strings.HasSuffix(u.Path, "/api/v3") && !strings.HasSuffix(u.Path, "/v3") {
		// /v1 is a common generic API-key default, not an Ark prefix.
		u.Path = strings.TrimSuffix(u.Path, "/v1") + "/api/v3"
	}
	u.Path += "/contents/generations/tasks"
	if id != "" {
		if validateUpstreamPathSegment("Seedance task ID", id) != nil {
			return "", errors.New("invalid Seedance task ID")
		}
		u.Path += "/" + id
	}
	return u.String(), nil
}

func (s *SeedanceService) request(ctx context.Context, a *Account, method, id string, body []byte) (int, []byte, error) {
	if a == nil || a.Platform != PlatformOpenAI || a.Type != AccountTypeAPIKey {
		return 0, nil, errors.New("Seedance owner is unavailable")
	}
	base, err := s.gateway.validateUpstreamBaseURL(a.GetCredential("base_url"))
	if err != nil {
		return 0, nil, err
	}
	target, err := buildSeedanceURL(base, id)
	if err != nil {
		return 0, nil, err
	}
	token := strings.TrimSpace(a.GetCredential("api_key"))
	if token == "" {
		return 0, nil, errors.New("Seedance credentials unavailable")
	}
	if method == http.MethodPost {
		ctx = WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileMedia)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	a.ApplyHeaderOverrides(req.Header)
	// These must never come from caller headers or account header overrides.
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	proxy := ""
	if a.Proxy != nil && a.ProxyID != nil {
		proxy = a.Proxy.URL()
	}
	resp, err := s.gateway.httpUpstream.Do(req, proxy, a.ID, a.Concurrency)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(data) > 4*1024*1024 {
		return resp.StatusCode, nil, errors.New("Seedance response too large")
	}
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, []byte(`{}`), nil
	}
	if !json.Valid(data) {
		return resp.StatusCode, nil, errors.New("invalid Seedance upstream JSON")
	}
	return resp.StatusCode, data, nil
}

func SeedanceIdempotencyID(key *APIKey, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) > 256 || strings.TrimSpace(value) == "" {
		return "", errors.New("Idempotency-Key must contain 1–256 characters")
	}
	groupID := int64(0)
	if key.GroupID != nil {
		groupID = *key.GroupID
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d:%s", key.UserID, key.ID, groupID, value)))
	return "seedance_" + hex.EncodeToString(sum[:]), nil
}

func (s *SeedanceService) Prepare(ctx context.Context, a *Account, key *APIKey, sub *UserSubscription, model, routingModel, billingModel, endpoint, operationID string, body []byte) (*SeedanceTask, error) {
	if !a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance) {
		return nil, errors.New("account does not support Seedance")
	}
	base, err := s.gateway.validateUpstreamBaseURL(a.GetCredential("base_url"))
	if err != nil {
		return nil, err
	}
	if _, err = buildSeedanceURL(base, ""); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.GetCredential("api_key")) == "" || strings.TrimSpace(a.GetMappedModel(routingModel)) == "" {
		return nil, errors.New("Seedance upstream credentials/model are incomplete")
	}
	resolved := s.gateway.resolver.Resolve(ctx, PricingInput{Model: billingModel, GroupID: key.GroupID, Group: key.Group})
	if !seedancePricingComplete(s.gateway.resolver, resolved) {
		return nil, ErrSeedancePricing
	}
	multiplier := 1.0
	if s.gateway.cfg != nil {
		multiplier = s.gateway.cfg.Default.RateMultiplier
	}
	if key.Group != nil && key.GroupID != nil {
		multiplier = key.Group.RateMultiplier
		if s.gateway.userGroupRateResolver != nil {
			multiplier = s.gateway.userGroupRateResolver.Resolve(ctx, key.UserID, *key.GroupID, multiplier)
		}
	}
	pricingAt := time.Now()
	_, _, multiplier = computePeakAwareMultipliers(key, multiplier, pricingAt)
	cost, err := s.gateway.billingService.CalculateCostUnified(CostInput{Ctx: ctx, Model: billingModel, Tokens: UsageTokens{OutputTokens: 1}, RequestCount: 1, UsageUnits: 1, RateMultiplier: multiplier, PricingAt: pricingAt, Resolved: resolved, Resolver: s.gateway.resolver})
	if err != nil || cost == nil {
		return nil, ErrSeedancePricing
	}
	for _, v := range []float64{cost.TotalCost, cost.ActualCost, multiplier, a.BillingRateMultiplier()} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, ErrSeedancePricing
		}
	}
	task := &SeedanceTask{ID: "seedance_" + uuid.NewString(), UserID: key.UserID, APIKeyID: key.ID, AccountID: a.ID, GroupID: key.GroupID, CreatedAt: time.Now(), State: "submitting"}
	if operationID != "" {
		task.ID = operationID
	}
	task.Snapshot = SeedanceSnapshot{Model: model, UpstreamModel: a.GetMappedModel(routingModel), BillingModel: billingModel, BillingMode: resolved.Mode, UnitTotal: cost.TotalCost, UnitActual: cost.ActualCost, RateMultiplier: multiplier, AccountMultiplier: a.BillingRateMultiplier(), KeyQuota: key.Quota > 0, KeyRateLimit: key.HasRateLimits(), AccountQuota: a.HasAnyQuotaLimit(), Platform: QuotaPlatform(ctx, key), InboundEndpoint: endpoint, PayloadHash: HashUsageRequestPayload(body)}
	task.Snapshot.UpstreamBaseURL = strings.TrimRight(base, "/")
	task.Snapshot.CredentialFingerprint = HashUsageRequestPayload([]byte(strings.TrimSpace(a.GetCredential("api_key"))))
	if sub != nil && key.Group != nil && key.Group.IsSubscriptionType() {
		task.Snapshot.SubscriptionID = &sub.ID
	}
	if s.gateway.cfg != nil {
		task.Snapshot.PlatformQuotaFlusher = s.gateway.cfg.Database.UserPlatformQuotaFlusherEnabled
	}
	limit := a.Concurrency
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	if raw := a.GetCredential("seedance_max_inflight"); raw != "" {
		if n, e := strconv.Atoi(raw); e == nil && n >= 1 && n <= 100 {
			limit = n
		}
	}
	if err = s.repo.Reserve(ctx, task, limit); err != nil {
		return nil, err
	}
	return task, nil
}

func seedancePricingComplete(resolver *ModelPricingResolver, pricing *ResolvedPricing) bool {
	if resolver == nil || pricing == nil || pricing.Source == PricingSourceFallback {
		return false
	}
	switch pricing.Mode {
	case BillingModePerRequest:
		if _, matched := resolver.getRequestTierPriceByContext(pricing, 0); matched {
			return true
		}
		return pricing.channelPricing != nil && pricing.channelPricing.PerRequestPrice != nil
	case BillingModeToken:
		base := resolver.GetIntervalPricing(pricing, 0)
		if base == nil {
			return false
		}
		if base.OutputPricePerToken > 0 {
			return true
		}
		// Distinguish intentional zero pricing from an empty configured entry.
		if pricing.channelPricing != nil && pricing.channelPricing.OutputPrice != nil {
			return true
		}
		interval := FindMatchingInterval(pricing.Intervals, 0)
		return interval != nil && interval.OutputPrice != nil
	default:
		return false
	}
}

// A create is submitted exactly once. Disconnecting the caller does not lose
// the provider ID; ambiguous outcomes remain durable and are never replayed.
func (s *SeedanceService) Create(ctx context.Context, a *Account, t *SeedanceTask, body []byte) (int, []byte, error) {
	deadline := time.Now().Add(2 * time.Minute)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	mapped, err := sjson.SetBytes(body, "model", t.Snapshot.UpstreamModel)
	if err != nil {
		return 0, nil, err
	}
	status, data, requestErr := s.request(ctx, a, http.MethodPost, "", mapped)
	state, provider := "submission_unknown", ""
	if requestErr == nil && status >= 200 && status < 300 {
		id := gjson.GetBytes(data, "id")
		provider = strings.TrimSpace(id.String())
		if id.Type != gjson.String || provider == "" || validateUpstreamPathSegment("Seedance task ID", provider) != nil {
			provider = ""
			requestErr = errors.New("Seedance create response missing a valid task ID")
		} else {
			state = "queued"
		}
	} else if requestErr == nil && status >= 400 && status < 500 && status != 408 {
		state = "rejected"
	}
	if state == "submission_unknown" && requestErr == nil {
		requestErr = errors.New("Seedance submission acceptance is unconfirmed")
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer writeCancel()
	if err = s.repo.Submitted(writeCtx, t.ID, provider, data, state); err != nil {
		// Provider ID is returned with the operation ID to aid reconciliation;
		// never turn this persistence failure into another upstream POST.
		slog.Error("seedance.submission_persist_failed", "operation_id", t.ID, "provider_id", provider, "error", err)
		return status, data, fmt.Errorf("persist Seedance submission: %w", err)
	}
	return status, data, requestErr
}

func (s *SeedanceService) Owned(ctx context.Context, id string, key *APIKey) (*SeedanceTask, error) {
	return s.repo.Owned(ctx, id, key.UserID, key.ID, key.GroupID)
}
func (s *SeedanceService) Delete(ctx context.Context, t *SeedanceTask) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if t.ProviderID == "" {
		return 409, nil, errors.New("submission outcome unknown; cannot cancel without provider ID")
	}
	a, err := s.gateway.accountRepo.GetByID(ctx, t.AccountID)
	if err != nil {
		return 0, nil, err
	}
	if err = s.validateTaskOwner(a, t); err != nil {
		return 409, nil, err
	}
	release, err := s.acquireLookupSlot(ctx, a)
	if err != nil {
		return 429, nil, err
	}
	defer release()
	// A completed provider task may be irretrievably removed by DELETE. Capture
	// its billable evidence first; the worker can then settle without the provider.
	if !t.Settled {
		state, _, complete := seedanceTaskTerminalUsage(t)
		if !complete {
			status, body, readErr := s.request(ctx, a, http.MethodGet, t.ProviderID, nil)
			if readErr != nil || status < 200 || status >= 300 || gjson.GetBytes(body, "id").Type != gjson.String || gjson.GetBytes(body, "id").String() != t.ProviderID {
				return 502, nil, errors.New("cannot verify Seedance task before cancellation")
			}
			t.Response = body
			state, _, complete = seedanceTaskTerminalUsage(t)
		}
		if complete {
			t.State = state
			if err := s.repo.SaveTerminal(ctx, t); err != nil {
				return 503, nil, err
			}
		} else if state != "queued" && state != "running" {
			return 409, nil, errors.New("Seedance terminal usage is unavailable; preserve task for reconciliation")
		}
	}
	// Running cancellation may still be billable; keep the local task for polling.
	return s.request(ctx, a, http.MethodDelete, t.ProviderID, nil)
}

func seedanceTaskTerminalUsage(t *SeedanceTask) (string, int, bool) {
	id := gjson.GetBytes(t.Response, "id")
	if t.ProviderID == "" || id.Type != gjson.String || id.String() != t.ProviderID {
		return "", 0, false
	}
	return seedanceTerminalUsage(t.Response)
}

func seedanceTerminalUsage(body []byte) (string, int, bool) {
	state := gjson.GetBytes(body, "status").String()
	switch state {
	case "succeeded", "failed", "cancelled", "expired":
	default:
		return state, 0, false
	}
	usage := gjson.GetBytes(body, "usage.completion_tokens")
	if usage.Type != gjson.Number {
		return state, 0, false
	}
	n, err := strconv.ParseInt(usage.Raw, 10, 64)
	// The existing usage_logs output_tokens column is PostgreSQL INT.
	// Keep oversized provider usage pending rather than truncating billing.
	if err != nil || n < 0 || n > math.MaxInt32 {
		return state, 0, false
	}
	return state, int(n), true
}

func (s *SeedanceService) settlement(t *SeedanceTask, tokens int) (*UsageBillingCommand, *UsageLog, error) {
	p := t.Snapshot
	units := float64(tokens)
	if p.BillingMode == BillingModePerRequest {
		units = 1
		if tokens == 0 && t.State != "succeeded" {
			units = 0
		}
	}
	total, actual := p.UnitTotal*units, p.UnitActual*units
	if math.IsNaN(total) || math.IsInf(total, 0) || math.IsNaN(actual) || math.IsInf(actual, 0) || total < 0 || actual < 0 || total >= 1e10 || actual >= 1e10 || total*p.AccountMultiplier >= 1e10 {
		return nil, nil, ErrSeedancePricing
	}
	endpoint := "/api/v3/contents/generations/tasks"
	mode := string(p.BillingMode)
	var durationMs *int
	if elapsed := time.Since(t.CreatedAt).Milliseconds(); elapsed >= 0 && elapsed <= math.MaxInt32 {
		duration := int(elapsed)
		durationMs = &duration
	}
	// Late reconciliation must not fail billing because duration_ms overflows;
	// original timestamps remain available in seedance_tasks.
	usage := &UsageLog{UserID: t.UserID, APIKeyID: t.APIKeyID, AccountID: t.AccountID, RequestID: t.ID, Model: p.BillingModel, RequestedModel: p.Model, UpstreamModel: &p.UpstreamModel, GroupID: t.GroupID, SubscriptionID: p.SubscriptionID, OutputTokens: tokens, OutputCost: total, TotalCost: total, ActualCost: actual, RateMultiplier: p.RateMultiplier, AccountRateMultiplier: &p.AccountMultiplier, BillingMode: &mode, RequestType: RequestTypeSync, DurationMs: durationMs, InboundEndpoint: &p.InboundEndpoint, UpstreamEndpoint: &endpoint, CreatedAt: time.Now()}
	media := "video"
	usage.MediaType = &media
	if t.State == "succeeded" {
		usage.VideoCount = 1
	}
	cmd := &UsageBillingCommand{RequestID: t.ID, APIKeyID: t.APIKeyID, UserID: t.UserID, AccountID: t.AccountID, AccountType: AccountTypeAPIKey, Model: p.BillingModel, OutputTokens: tokens, SubscriptionID: p.SubscriptionID, RequestPayloadHash: p.PayloadHash, MediaType: "video"}
	if p.SubscriptionID != nil {
		cmd.SubscriptionCost = actual
		cmd.BillingType = BillingTypeSubscription
		usage.BillingType = BillingTypeSubscription
	} else {
		cmd.BalanceCost = actual
	}
	if p.KeyQuota {
		cmd.APIKeyQuotaCost = actual
	}
	if p.KeyRateLimit {
		cmd.APIKeyRateLimitCost = actual
	}
	if p.AccountQuota {
		cmd.AccountQuotaCost = total * p.AccountMultiplier
	}
	return cmd, usage, nil
}

func (s *SeedanceService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	for i := 0; i < 4; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.pollOne(ctx)
				}
			}
		}()
	}
}
func (s *SeedanceService) Stop() {
	if s != nil && s.cancel != nil {
		s.cancel()
		s.wg.Wait()
	}
}
func (s *SeedanceService) pollOne(parent context.Context) {
	defer func() {
		if value := recover(); value != nil {
			slog.Error("seedance.worker_panic", "panic", fmt.Sprint(value))
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	t, err := s.repo.Claim(ctx)
	if err != nil {
		if !errors.Is(err, ErrSeedanceTaskNotFound) && ctx.Err() == nil {
			slog.Warn("seedance.claim_failed", "error", err)
		}
		return
	}
	if t.Settled {
		if err = s.completeEffects(ctx, t); err != nil {
			slog.Warn("seedance.effects_pending", "operation_id", t.ID, "error", err)
		}
		return
	}
	if state, _, complete := seedanceTaskTerminalUsage(t); complete {
		t.State = state
		s.settleObserved(ctx, t)
		return
	}
	a, err := s.gateway.accountRepo.GetByID(ctx, t.AccountID)
	if err != nil {
		slog.Warn("seedance.owner_unavailable", "operation_id", t.ID, "error", err)
		return
	}
	if err = s.validateTaskOwner(a, t); err != nil {
		slog.Error("seedance.owner_changed", "operation_id", t.ID, "error", err)
		return
	}
	release, err := s.acquireLookupSlot(ctx, a)
	if err != nil {
		return
	}
	defer release()
	status, body, err := s.request(ctx, a, http.MethodGet, t.ProviderID, nil)
	delay := time.Duration(min(300, 5+t.Attempts*5)) * time.Second
	if err != nil || status < 200 || status >= 300 {
		_ = s.repo.Observe(ctx, t.ID, nil, "", delay)
		return
	}
	if responseID := gjson.GetBytes(body, "id"); responseID.Type != gjson.String || responseID.String() != t.ProviderID {
		slog.Error("seedance.response_id_mismatch", "operation_id", t.ID)
		_ = s.repo.Observe(ctx, t.ID, nil, "", delay)
		return
	}
	state, _, complete := seedanceTerminalUsage(body)
	if !complete {
		_ = s.repo.Observe(ctx, t.ID, body, state, delay)
		return
	}
	t.State = state
	t.Response = body
	s.settleObserved(ctx, t)
}

func (s *SeedanceService) settleObserved(ctx context.Context, t *SeedanceTask) {
	if err := s.repo.SaveTerminal(ctx, t); err != nil {
		slog.Error("seedance.terminal_persist_failed", "operation_id", t.ID, "error", err)
		return
	}
	state, tokens, complete := seedanceTaskTerminalUsage(t)
	if !complete {
		return
	}
	t.State = state
	if s.gateway.cfg != nil && t.Snapshot.PlatformQuotaFlusher != s.gateway.cfg.Database.UserPlatformQuotaFlusherEnabled {
		// Do not apply an old task using the other quota persistence strategy.
		// Keep its usage durable for reconciliation during configuration changes.
		slog.Error("seedance.quota_mode_changed", "operation_id", t.ID)
		return
	}
	cmd, usage, err := s.settlement(t, tokens)
	if err != nil {
		slog.Error("seedance.pricing_failed", "operation_id", t.ID, "error", err)
		return
	}
	_, err = s.repo.Settle(ctx, t, cmd, usage)
	if err != nil {
		slog.Warn("seedance.settlement_pending", "operation_id", t.ID, "error", err)
		return
	}
	t.Settled = true
	t.EffectsPending = true
	if err = s.completeEffects(ctx, t); err != nil {
		slog.Warn("seedance.effects_pending", "operation_id", t.ID, "error", err)
	}
}

func (s *SeedanceService) acquireLookupSlot(ctx context.Context, a *Account) (func(), error) {
	if s.gateway.concurrencyService == nil {
		return func() {}, nil
	}
	slot, err := s.gateway.concurrencyService.AcquireAccountSlotForPlatform(ctx, a.Platform, a.ID, a.Concurrency)
	if err != nil {
		return nil, err
	}
	if slot == nil || !slot.Acquired {
		return nil, ErrSeedanceCapacity
	}
	if slot.ReleaseFunc == nil {
		return func() {}, nil
	}
	return slot.ReleaseFunc, nil
}

func (s *SeedanceService) completeEffects(ctx context.Context, t *SeedanceTask) error {
	if s.gateway.cfg != nil && t.Snapshot.PlatformQuotaFlusher != s.gateway.cfg.Database.UserPlatformQuotaFlusherEnabled {
		return errors.New("Seedance quota persistence mode changed; reconcile pending task before switching modes")
	}
	cache := s.gateway.billingCacheService
	if cache != nil {
		if t.Snapshot.SubscriptionID == nil && t.Snapshot.Platform != "" {
			_, tokens, complete := seedanceTerminalUsage(t.Response)
			if !complete {
				return errors.New("Seedance terminal usage missing")
			}
			_, usage, err := s.settlement(t, tokens)
			if err != nil {
				return err
			}
			if usage.ActualCost > 0 {
				if t.Snapshot.PlatformQuotaFlusher {
					if err = s.deliverPlatformQuota(ctx, t, usage.ActualCost); err != nil {
						return err
					}
				} else if cache.cache != nil {
					if err = cache.cache.DeleteUserPlatformQuotaCache(ctx, t.UserID, t.Snapshot.Platform); err != nil {
						return err
					}
				}
			}
		}
		if err := cache.InvalidateUserBalance(ctx, t.UserID); err != nil {
			return err
		}
		if t.GroupID != nil && t.Snapshot.SubscriptionID != nil {
			if err := cache.InvalidateSubscription(ctx, t.UserID, *t.GroupID); err != nil {
				return err
			}
		}
		if err := cache.InvalidateAPIKeyRateLimit(ctx, t.APIKeyID); err != nil {
			return err
		}
	}
	if s.keys != nil {
		s.keys.InvalidateAuthCacheByUserID(ctx, t.UserID)
	}
	if s.gateway.deferredService != nil {
		s.gateway.deferredService.ScheduleLastUsedUpdate(t.AccountID)
	}
	if err := s.repo.CompleteEffects(ctx, t.ID); err != nil {
		return err
	}
	if cache != nil {
		if acknowledger, ok := cache.cache.(interface {
			AcknowledgeSeedancePlatformQuota(context.Context, string) error
		}); ok {
			// Only after durable acknowledgement is it safe to expire the marker.
			if err := acknowledger.AcknowledgeSeedancePlatformQuota(ctx, t.ID); err != nil {
				slog.Warn("seedance.quota_marker_retained", "operation_id", t.ID, "error", err)
			}
		}
	}
	return nil
}

func (s *SeedanceService) validateTaskOwner(account *Account, task *SeedanceTask) error {
	if account == nil {
		return errors.New("Seedance owner missing")
	}
	base, err := s.gateway.validateUpstreamBaseURL(account.GetCredential("base_url"))
	if err != nil {
		return err
	}
	fingerprint := HashUsageRequestPayload([]byte(strings.TrimSpace(account.GetCredential("api_key"))))
	if strings.TrimRight(base, "/") != task.Snapshot.UpstreamBaseURL || fingerprint != task.Snapshot.CredentialFingerprint {
		return errors.New("Seedance upstream identity changed; pending task requires reconciliation")
	}
	return nil
}

// Redis acknowledgement is idempotent so an outbox retry after an uncertain
// reply cannot charge the platform quota twice. The existing flusher continues
// to own persistence when enabled; the transaction owns it otherwise.
func (s *SeedanceService) deliverPlatformQuota(ctx context.Context, t *SeedanceTask, cost float64) error {
	cache := s.gateway.billingCacheService
	writer, ok := cache.cache.(interface {
		ApplySeedancePlatformQuota(context.Context, string, int64, string, float64, time.Duration) error
	})
	if !ok {
		return errors.New("Seedance platform quota outbox unavailable")
	}
	// Hydrate/reset the existing quota cache before atomically applying the event.
	_ = cache.checkUserPlatformQuotaEligibility(ctx, t.UserID, t.Snapshot.Platform)
	ttl := time.Minute
	if cache.cfg != nil {
		ttl = time.Duration(cache.cfg.Billing.UserPlatformQuotaCacheTTLSeconds) * time.Second
	}
	return writer.ApplySeedancePlatformQuota(ctx, t.ID, t.UserID, t.Snapshot.Platform, cost, ttl)
}
