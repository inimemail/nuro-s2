package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type GeminiCacheProvider interface {
	CountCacheTokens(context.Context, *Account, string, map[string]json.RawMessage) (int64, error)
	CreateCacheContent(context.Context, *Account, string, string, *GeminiCacheCreateRequest) (*GeminiCacheProviderResult, error)
	GetCacheContent(context.Context, *Account, string) (*GeminiCacheProviderResult, error)
	PatchCacheContent(context.Context, *Account, string, time.Time) (*GeminiCacheProviderResult, error)
	DeleteCacheContent(context.Context, *Account, string) error
	FindCacheOperation(context.Context, *Account, string) (*GeminiCacheProviderResult, error)
}

type GeminiCachedContentService struct {
	repo          GeminiCachedContentRepository
	charges       GeminiCacheChargeRepository
	publisher     GeminiCacheAdmissionPublisher
	provider      GeminiCacheProvider
	accounts      AccountRepository
	gateway       *GatewayService
	billing       *BillingService
	resolver      *ModelPricingResolver
	view          *GeminiCacheResourceView
	cfg           *config.Config
	node          string
	enabled       bool
	serveExisting atomic.Bool
	maxResources  int
	maxTTL        time.Duration
	authBarrier   time.Duration
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	running       atomic.Bool
	stopped       atomic.Bool
	writes        sync.WaitGroup
	writesMu      sync.Mutex
	writeCount    int
	ownerWrites   map[int64]int
}

func NewGeminiCachedContentService(repo GeminiCachedContentRepository, charges GeminiCacheChargeRepository, publisher GeminiCacheAdmissionPublisher, provider *GeminiMessagesCompatService, accounts AccountRepository, gateway *GatewayService, billing *BillingService, resolver *ModelPricingResolver, cfg *config.Config) *GeminiCachedContentService {
	s := &GeminiCachedContentService{repo: repo, charges: charges, publisher: publisher, provider: provider, accounts: accounts, gateway: gateway, billing: billing, resolver: resolver, cfg: cfg, node: newGeminiCacheID(), maxResources: 10000, maxTTL: 24 * time.Hour}
	if cfg != nil {
		s.enabled = cfg.Gateway.GeminiExplicitCacheEnabled
		if cfg.Gateway.GeminiExplicitCacheMaxResources > 0 {
			s.maxResources = min(cfg.Gateway.GeminiExplicitCacheMaxResources, 100000)
		}
		if cfg.Gateway.GeminiExplicitCacheMaxTTLSeconds > 0 {
			s.maxTTL = time.Duration(cfg.Gateway.GeminiExplicitCacheMaxTTLSeconds) * time.Second
		}
		s.authBarrier = time.Duration(cfg.APIKeyAuth.L1TTLSeconds)*time.Second*(100+time.Duration(min(max(cfg.APIKeyAuth.JitterPercent, 0), 100)))/100 + time.Second
	}
	s.view = NewGeminiCacheResourceView(s.maxResources)
	return s
}

func (s *GeminiCachedContentService) Enabled() bool {
	return s != nil && (s.enabled || s.serveExisting.Load())
}
func (s *GeminiCachedContentService) ResolveBinding(owner GeminiCacheOwner, id, model string) (GeminiCacheBinding, error) {
	if !s.Enabled() {
		return GeminiCacheBinding{}, ErrGeminiCacheUnavailable
	}
	return s.view.Lookup(owner, id, model)
}
func (s *GeminiCachedContentService) managementContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if !s.Enabled() || !s.running.Load() || s.stopped.Load() || s.publisher == nil || s.authBarrier > 30*time.Second || !s.view.Ready() {
		return nil, nil, ErrGeminiCacheUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	return ctx, cancel, nil
}
func (s *GeminiCachedContentService) enterWrite(owner GeminiCacheOwner) error {
	s.writesMu.Lock()
	defer s.writesMu.Unlock()
	if s.stopped.Load() {
		return ErrGeminiCacheUnavailable
	}
	if s.writeCount >= 64 || s.ownerWrites[owner.UserID] >= 2 {
		return ErrGeminiCacheUnavailable
	}
	if s.ownerWrites == nil {
		s.ownerWrites = make(map[int64]int)
	}
	s.writeCount++
	s.ownerWrites[owner.UserID]++
	s.writes.Add(1)
	return nil
}
func (s *GeminiCachedContentService) leaveWrite(owner GeminiCacheOwner) {
	s.writesMu.Lock()
	s.writeCount--
	s.ownerWrites[owner.UserID]--
	if s.ownerWrites[owner.UserID] == 0 {
		delete(s.ownerWrites, owner.UserID)
	}
	s.writesMu.Unlock()
	s.writes.Done()
}
func GeminiCacheOwnerForKey(key *APIKey) (GeminiCacheOwner, error) {
	if key == nil || key.User == nil || key.Group == nil || key.GroupID == nil || key.Group.Platform != PlatformGemini || *key.GroupID != key.Group.ID {
		return GeminiCacheOwner{}, ErrGeminiCacheNotFound
	}
	return GeminiCacheOwner{key.UserID, key.ID, key.Group.ID}, nil
}

func (s *GeminiCachedContentService) Create(ctx context.Context, key *APIKey, sub *UserSubscription, body []byte, idempotency string) (*GeminiCachedContent, *GeminiCacheOperation, error) {
	return s.create(ctx, key, sub, body, idempotency, false)
}

// Preview performs the same provider count and pricing selection without a
// reservation or provider mutation. The real operation always requotes.
func (s *GeminiCachedContentService) Preview(ctx context.Context, key *APIKey, sub *UserSubscription, body []byte) (*GeminiCacheQuote, error) {
	_, op, err := s.create(ctx, key, sub, body, "", true)
	if err != nil {
		return nil, err
	}
	return &op.Quote, nil
}

func (s *GeminiCachedContentService) create(ctx context.Context, key *APIKey, sub *UserSubscription, body []byte, idempotency string, preview bool) (*GeminiCachedContent, *GeminiCacheOperation, error) {
	owner, err := GeminiCacheOwnerForKey(key)
	if err != nil {
		return nil, nil, err
	}
	hash, err := GeminiCacheIdempotencyHash(idempotency)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrGeminiCacheInvalidRequest, err)
	}
	ctx, cancel, err := s.managementContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer cancel()
	if !preview && hash != "" {
		if existing, lookupErr := s.repo.FindOperation(ctx, owner, "create", hash); lookupErr != nil {
			return nil, nil, lookupErr
		} else if existing != nil {
			if existing.Fingerprint != GeminiCacheFingerprint(owner, "create", "", body) {
				return nil, nil, ErrGeminiCacheConflict
			}
			return s.operationResponse(ctx, existing)
		}
	}
	// Resolve recorded operations before checking today's TTL or creation
	// switch. A retry must report its original result even after expiry or
	// entry into maintenance, and must never issue another provider mutation.
	if !s.enabled || (s.cfg != nil && s.cfg.Gateway.GeminiExplicitCacheMaintenance) {
		return nil, nil, ErrGeminiCacheUnavailable
	}
	request, err := ParseGeminiCacheCreate(body, time.Now(), s.maxTTL)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrGeminiCacheInvalidRequest, err)
	}
	if err = s.enterWrite(owner); err != nil {
		return nil, nil, err
	}
	defer s.leaveWrite(owner)
	mapping, restricted := s.gateway.ResolveChannelMappingAndRestrict(ctx, key.GroupID, request.Model)
	if restricted {
		return nil, nil, ErrNoAvailableAccounts
	}
	mapped := request.Model
	if mapping.Mapped {
		mapped = mapping.MappedModel
	}
	selected, err := s.gateway.SelectAccountWithLoadAwareness(withGeminiCacheCandidates(ctx), key.GroupID, "", mapped, nil, "", 0)
	if err != nil || selected == nil || selected.Account == nil {
		return nil, nil, ErrNoAvailableAccounts
	}
	if selected.ReleaseFunc != nil {
		defer selected.ReleaseFunc()
	}
	if !selected.Acquired {
		return nil, nil, ErrGeminiCacheUnavailable
	}
	account := selected.Account
	if !IsExplicitCacheAccount(account) {
		return nil, nil, errors.New("selected account does not support explicit caching")
	}
	if request.ExpiresAt.Sub(time.Now()) > GeminiCacheAccountMaxTTL(account) {
		return nil, nil, ErrGeminiCacheInvalidRequest
	}
	upstream := account.GetMappedModel(mapped)
	tokens, err := s.provider.CountCacheTokens(ctx, account, upstream, request.Body)
	if err != nil {
		return nil, nil, err
	}
	billingModel := mapped
	switch mapping.BillingModelSource {
	case BillingModelSourceRequested:
		billingModel = request.Model
	case BillingModelSourceUpstream:
		billingModel = upstream
	}
	at := time.Now()
	pricing, err := s.pricingContext(ctx, key, account, billingModel, upstream, tokens, at)
	if err != nil {
		return nil, nil, err
	}
	if account.IsVertexServiceAccount() {
		pricing.ProviderProject = account.VertexProjectID()
		pricing.ProviderLocation = account.VertexLocation(upstream)
	}
	quote, err := QuoteGeminiCache(pricing, tokens, at, request.ExpiresAt, true)
	if err != nil {
		return nil, nil, err
	}
	resource := &GeminiCachedContent{ID: newGeminiCacheID(), Owner: owner, AccountID: account.ID, RequestedModel: request.Model, UpstreamModel: upstream, BillingModel: billingModel, DisplayName: request.DisplayName, State: "pending", TokenCount: tokens, ExpiresAt: request.ExpiresAt, PaidUntil: at, Pricing: pricing}
	op := &GeminiCacheOperation{ID: newGeminiCacheID(), ResourceID: resource.ID, Owner: owner, AccountID: account.ID, Kind: "create", IdempotencyKeyHash: hash, Fingerprint: GeminiCacheFingerprint(owner, "create", "", body), TargetExpiry: request.ExpiresAt, Quote: quote, LeaseOwner: s.node}
	if sub != nil && key.Group.IsSubscriptionType() {
		id := sub.ID
		op.SubscriptionID = &id
	}
	if preview {
		return resource, op, nil
	}
	prepared, err := s.repo.Prepare(ctx, resource, op, s.maxResources)
	if err != nil {
		return nil, nil, err
	}
	if prepared.ID != op.ID {
		return s.operationResponse(ctx, prepared)
	}
	if err = s.reserveAndFence(ctx, op, resource.Revision); err != nil {
		s.abandonUnsent(op)
		return nil, op, err
	}
	if err = s.repo.MarkSendStarted(ctx, op); err != nil {
		s.abandonUnsent(op)
		return nil, op, err
	}
	result, err := s.provider.CreateCacheContent(ctx, account, upstream, op.ID, request)
	if err != nil {
		return nil, op, s.providerFailure(op, err)
	}
	if err = validateCacheProviderBinding(account, upstream, result); err != nil {
		return nil, op, s.providerFailure(op, &GeminiCacheProviderError{Uncertain: true})
	}
	if err = s.confirmAndSettle(ctx, op, result); err != nil {
		return nil, op, err
	}
	return s.operationResponse(ctx, op)
}

func (s *GeminiCachedContentService) reserveAndFence(ctx context.Context, op *GeminiCacheOperation, revision int64) error {
	if err := WaitGeminiCacheBarrier(ctx, s.repo, revision); err != nil {
		return err
	}
	if err := s.charges.ReserveCacheCharge(ctx, op); err != nil {
		return err
	}
	if op.Quote.Amount == "0.00000000" {
		return nil
	}
	if err := s.publisher.PublishCacheChargeAdmission(ctx, op); err != nil {
		return err
	}
	// Old L1 entries are bounded from the beginning of their original lookup,
	// including a delayed refill. This wait belongs only to paid management.
	timer := time.NewTimer(s.authBarrier)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (s *GeminiCachedContentService) operationResponse(ctx context.Context, op *GeminiCacheOperation) (*GeminiCachedContent, *GeminiCacheOperation, error) {
	resource, err := s.repo.GetForOwner(ctx, op.Owner, op.ResourceID)
	if err != nil {
		return nil, op, err
	}
	if op.State != "settled" {
		if op.State == "released" {
			return resource, op, ErrGeminiCacheRejected
		}
		return resource, op, ErrGeminiCacheConflict
	}
	return resource, op, nil
}
func (s *GeminiCachedContentService) abandonUnsent(op *GeminiCacheOperation) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.charges.ReleaseCacheCharge(ctx, op); err != nil {
		return
	}
	if op.Quote.Amount != "0.00000000" {
		if err := s.publisher.PublishCacheChargeAdmission(ctx, op); err != nil {
			return
		}
	}
	_ = s.repo.Finish(ctx, op, "released")
}
func (s *GeminiCachedContentService) providerFailure(op *GeminiCacheOperation, err error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A proven rejection authorizes release; unknown execution is never replayed.
	var failure *GeminiCacheProviderError
	if errors.As(err, &failure) && !failure.Uncertain && failure.Status >= 400 && failure.Status < 500 {
		if s.repo.Reject(ctx, op) == nil {
			s.abandonUnsent(op)
		}
	} else {
		_ = s.repo.Retry(ctx, op, "provider_result_unknown", time.Now().Add(time.Minute), true)
		return ErrGeminiCacheConflict
	}
	return err
}
func (s *GeminiCachedContentService) confirmAndSettle(ctx context.Context, op *GeminiCacheOperation, result *GeminiCacheProviderResult) error {
	// Finish finance even when a client disconnects after the upstream write.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := s.repo.Confirm(ctx, op, result.Name, result.Tokens, result.ExpiresAt); err != nil {
		return err
	}
	return s.settleConfirmed(ctx, op)
}
func (s *GeminiCachedContentService) settleConfirmed(ctx context.Context, op *GeminiCacheOperation) error {
	actual, err := ConfirmedGeminiCacheQuote(op)
	if err != nil {
		return err
	}
	if err = s.charges.CaptureCacheCharge(ctx, op, actual); err != nil {
		_ = s.repo.Retry(ctx, op, "settlement_requires_reconciliation", time.Now().Add(time.Minute), true)
		return err
	}
	if op.Quote.Amount != "0.00000000" || actual.Amount != "0.00000000" {
		if err = s.publisher.PublishCacheChargeAdmission(ctx, op); err != nil {
			return err
		}
	}
	return s.repo.Finish(ctx, op, "settled")
}

func (s *GeminiCachedContentService) Get(ctx context.Context, owner GeminiCacheOwner, id string) (*GeminiCachedContent, error) {
	return s.repo.GetForOwner(ctx, owner, id)
}
func (s *GeminiCachedContentService) List(ctx context.Context, owner GeminiCacheOwner, page string, size int) ([]GeminiCachedContent, string, error) {
	return s.repo.ListForOwner(ctx, owner, page, size)
}

func ConfirmedGeminiCacheQuote(op *GeminiCacheOperation) (GeminiCacheQuote, error) {
	if op == nil {
		return GeminiCacheQuote{}, ErrGeminiCacheConflict
	}
	if op.ConfirmedTokens == nil || op.ConfirmedExpiry == nil {
		return GeminiCacheQuote{}, ErrGeminiCacheConflict
	}
	until := *op.ConfirmedExpiry
	if until.Before(op.Quote.PaidFrom) {
		until = op.Quote.PaidFrom
	}
	actual, err := QuoteGeminiCache(op.Quote.Pricing, *op.ConfirmedTokens, op.Quote.PaidFrom, until, op.Kind == "create")
	if err != nil {
		return GeminiCacheQuote{}, err
	}
	if op.Kind == "delete" {
		actual.Amount = "0.00000000"
		actual.AccountAmount = "0.00000000"
		actual.InputCost = "0"
		actual.StorageCost = "0"
	}
	return actual, nil
}
