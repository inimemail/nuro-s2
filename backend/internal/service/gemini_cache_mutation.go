package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func validateCacheProviderBinding(account *Account, model string, result *GeminiCacheProviderResult) error {
	if result == nil || result.Tokens <= 0 || result.Tokens > 1_000_000_000 || lastSegment(result.Model) != model {
		return ErrGeminiCacheConflict
	}
	if account.IsVertexServiceAccount() {
		prefix := "projects/" + account.VertexProjectID() + "/locations/" + account.VertexLocation(model) + "/"
		if !strings.HasPrefix(result.Name, prefix+"cachedContents/") || result.Model != prefix+"publishers/google/models/"+model {
			return ErrGeminiCacheConflict
		}
	} else if !strings.HasPrefix(result.Name, "cachedContents/") {
		return ErrGeminiCacheConflict
	}
	return nil
}

func validateExistingCacheProviderBinding(account *Account, resource *GeminiCachedContent, result *GeminiCacheProviderResult) error {
	if resource == nil || result == nil || result.Name != resource.UpstreamName || result.Tokens != resource.TokenCount {
		return ErrGeminiCacheConflict
	}
	if account.IsVertexServiceAccount() {
		prefix, _, ok := strings.Cut(resource.UpstreamName, "/cachedContents/")
		if !ok || !geminiCacheUpstreamNamePattern.MatchString(resource.UpstreamName) || !strings.HasPrefix(prefix, "projects/"+account.VertexProjectID()+"/locations/") || result.Model != prefix+"/publishers/google/models/"+resource.UpstreamModel {
			return ErrGeminiCacheConflict
		}
		return nil
	}
	return validateCacheProviderBinding(account, resource.UpstreamModel, result)
}

func (s *GeminiCachedContentService) Mutate(ctx context.Context, key *APIKey, sub *UserSubscription, id string, body []byte, mask, idempotency string, remove bool) (*GeminiCachedContent, *GeminiCacheOperation, error) {
	return s.mutate(ctx, key, sub, id, body, mask, idempotency, remove, false)
}

func (s *GeminiCachedContentService) PreviewMutation(ctx context.Context, key *APIKey, sub *UserSubscription, id string, body []byte, mask string) (*GeminiCacheQuote, error) {
	_, op, err := s.mutate(ctx, key, sub, id, body, mask, "", false, true)
	if err != nil {
		return nil, err
	}
	return &op.Quote, nil
}

func (s *GeminiCachedContentService) mutate(ctx context.Context, key *APIKey, sub *UserSubscription, id string, body []byte, mask, idempotency string, remove, preview bool) (*GeminiCachedContent, *GeminiCacheOperation, error) {
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
	kind := "patch"
	if remove {
		kind = "delete"
	}
	if hash != "" {
		if existing, lookupErr := s.repo.FindOperation(ctx, owner, kind, hash); lookupErr != nil {
			return nil, nil, lookupErr
		} else if existing != nil {
			if existing.Fingerprint != GeminiCacheFingerprint(owner, kind, id, body) {
				return nil, nil, ErrGeminiCacheConflict
			}
			return s.operationResponse(ctx, existing)
		}
	}
	if err = s.enterWrite(owner); err != nil {
		return nil, nil, err
	}
	defer s.leaveWrite(owner)
	resource, err := s.repo.GetForOwner(ctx, owner, id)
	if err != nil {
		return nil, nil, err
	}
	if remove && resource.State == "deleted" {
		return resource, nil, nil
	}
	if resource.State != "active" {
		return nil, nil, ErrGeminiCacheConflict
	}
	target := resource.ExpiresAt
	if !remove {
		if !s.enabled || (s.cfg != nil && s.cfg.Gateway.GeminiExplicitCacheMaintenance) {
			return nil, nil, ErrGeminiCacheUnavailable
		}
		target, err = ParseGeminiCachePatch(body, mask, time.Now(), s.maxTTL)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrGeminiCacheInvalidRequest, err)
		}
	}
	mapping, restricted := s.gateway.ResolveChannelMappingAndRestrict(ctx, key.GroupID, resource.RequestedModel)
	if restricted {
		return nil, nil, ErrNoAvailableAccounts
	}
	mapped := resource.RequestedModel
	if mapping.Mapped {
		mapped = mapping.MappedModel
	}
	selected, err := s.gateway.SelectRequiredAccountWithLoadAwareness(ctx, key.GroupID, resource.AccountID, "", mapped, nil)
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
	if !IsExplicitCacheAccount(account) || account.GetMappedModel(mapped) != resource.UpstreamModel {
		return nil, nil, errors.New("cache account capability or model mapping changed")
	}
	if !remove && target.Sub(time.Now()) > GeminiCacheAccountMaxTTL(account) {
		return nil, nil, ErrGeminiCacheInvalidRequest
	}
	paidFrom := resource.PaidUntil
	until := target
	if remove || until.Before(paidFrom) {
		until = paidFrom
	}
	quote, err := QuoteGeminiCache(resource.Pricing, resource.TokenCount, paidFrom, until, false)
	if err != nil {
		return nil, nil, err
	}
	baseline := resource.ExpiresAt
	op := &GeminiCacheOperation{ID: newGeminiCacheID(), ResourceID: id, Owner: owner, AccountID: resource.AccountID, Kind: kind, IdempotencyKeyHash: hash, Fingerprint: GeminiCacheFingerprint(owner, kind, id, body), BaselineExpiry: &baseline, TargetExpiry: target, Quote: quote, UpstreamName: resource.UpstreamName, LeaseOwner: s.node}
	if sub != nil && key.Group.IsSubscriptionType() {
		sid := sub.ID
		op.SubscriptionID = &sid
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
	result := &GeminiCacheProviderResult{Name: resource.UpstreamName, Model: resource.UpstreamModel, Tokens: resource.TokenCount, ExpiresAt: target}
	if remove {
		err = s.provider.DeleteCacheContent(ctx, account, resource.UpstreamName)
	} else {
		result, err = s.provider.PatchCacheContent(ctx, account, resource.UpstreamName, target)
	}
	if err != nil {
		return nil, op, s.providerFailure(op, err)
	}
	if !remove {
		if err = validateExistingCacheProviderBinding(account, resource, result); err != nil {
			return nil, op, s.providerFailure(op, &GeminiCacheProviderError{Uncertain: true})
		}
	}
	if err = s.confirmAndSettle(ctx, op, result); err != nil {
		return nil, op, err
	}
	return s.operationResponse(ctx, op)
}
