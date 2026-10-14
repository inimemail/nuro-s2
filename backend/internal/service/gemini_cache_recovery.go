package service

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

func (s *GeminiCachedContentService) Start() {
	if s == nil || s.repo == nil || s.charges == nil || s.publisher == nil || !s.running.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	if !s.enabled {
		if state, ok := s.repo.(interface {
			HasCacheState(context.Context) (bool, error)
		}); ok {
			checkCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			exists, err := state.HasCacheState(checkCtx)
			stop()
			if err == nil && !exists {
				return
			}
			s.serveExisting.Store(true)
		}
	}
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		if !s.Enabled() {
			return
		}
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		bootstrap := true
		for {
			refreshCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			if err := s.view.Refresh(refreshCtx, s.repo, s.node, bootstrap); err == nil {
				bootstrap = false
			} else {
				bootstrap = true
			}
			stop()
			select {
			case <-ctx.Done():
				s.view.Invalidate()
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			s.recoverBatch(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (s *GeminiCachedContentService) Stop() {
	if s == nil {
		return
	}
	s.writesMu.Lock()
	s.stopped.Store(true)
	s.writesMu.Unlock()
	s.writes.Wait()
	// Stop waits are bounded by operation contexts. Intents survive a forced
	// process exit and are claimed by the next node after the lease safety gap.
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	s.view.Invalidate()
}
func (s *GeminiCachedContentService) recoverBatch(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	if maintenance, ok := s.repo.(interface{ PruneCacheMetadata(context.Context) error }); ok {
		_ = maintenance.PruneCacheMetadata(ctx)
	}
	ops, err := s.repo.ClaimRecovery(ctx, s.node, 50, 120*time.Second)
	if err != nil || len(ops) == 0 {
		return
	}
	jobs := make(chan GeminiCacheOperation, len(ops))
	for _, op := range ops {
		jobs <- op
	}
	close(jobs)
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for op := range jobs {
				if ctx.Err() != nil {
					return
				}
				itemCtx, stop := context.WithTimeout(ctx, 45*time.Second)
				s.recoverOperation(itemCtx, &op)
				stop()
			}
		}()
	}
	workers.Wait()
}
func (s *GeminiCachedContentService) recoverOperation(ctx context.Context, op *GeminiCacheOperation) {
	retry := func(code string, reconcile bool) {
		delay := time.Duration(1<<min(op.Attempts, 8))*time.Minute + time.Duration(rand.IntN(30))*time.Second
		_ = s.repo.Retry(ctx, op, code, time.Now().Add(delay), reconcile)
	}
	if op.State == "prepared" || op.State == "reserved" {
		if err := s.charges.ReleaseCacheCharge(ctx, op); err != nil {
			retry("release_pending", false)
			return
		}
		if op.Quote.Amount != "0.00000000" {
			if err := s.publisher.PublishCacheChargeAdmission(ctx, op); err != nil {
				retry("admission_publish_pending", false)
				return
			}
		}
		if err := s.repo.Finish(ctx, op, "released"); err != nil {
			retry("release_commit_pending", false)
		}
		return
	}
	if op.State == "upstream_confirmed" || op.State == "settling" {
		if err := s.settleConfirmed(ctx, op); err != nil {
			retry("settlement_pending", false)
		}
		return
	}
	if op.ConfirmedExpiry != nil && op.ConfirmedTokens != nil {
		// Do not turn an over-quote amount into a silent refund. The stored result
		// remains available for controlled deficit reconciliation.
		retry("confirmed_cost_requires_reconciliation", true)
		return
	}
	account, err := s.accounts.GetByID(ctx, op.AccountID)
	if err != nil || !IsExplicitCacheAccount(account) || account.Status != StatusActive || !account.Schedulable {
		retry("account_unavailable", false)
		return
	}
	resource, err := s.repo.GetForOwner(ctx, op.Owner, op.ResourceID)
	if err != nil {
		retry("resource_unavailable", true)
		return
	}
	var result *GeminiCacheProviderResult
	switch op.Kind {
	case "create":
		searchAccount := account
		if account.IsVertexServiceAccount() {
			location := resource.Pricing.ProviderLocation
			if location == "" {
				location = account.VertexLocation(resource.UpstreamModel)
			}
			if resource.Pricing.ProviderProject != "" && resource.Pricing.ProviderProject != account.VertexProjectID() {
				retry("provider_project_changed", true)
				return
			}
			copy := *account
			copy.Credentials = cloneCredentials(account.Credentials)
			copy.Credentials["location"] = location
			delete(copy.Credentials, "vertex_model_locations")
			searchAccount = &copy
		}
		result, err = s.provider.FindCacheOperation(ctx, searchAccount, op.ID)
		if err != nil {
			retry("create_result_unknown", true)
			return
		}
		if err = validateCacheProviderBinding(searchAccount, resource.UpstreamModel, result); err != nil {
			retry("provider_binding_conflict", true)
			return
		}
	case "patch", "delete":
		result, err = s.provider.GetCacheContent(ctx, account, resource.UpstreamName)
		var providerErr *GeminiCacheProviderError
		if op.Kind == "delete" && errors.As(err, &providerErr) && providerErr.ConfirmedNotFound {
			result = &GeminiCacheProviderResult{Name: resource.UpstreamName, Tokens: resource.TokenCount, ExpiresAt: op.TargetExpiry}
			err = nil
		} else if err != nil {
			retry("mutation_result_unknown", true)
			return
		} else if err = validateExistingCacheProviderBinding(account, resource, result); err != nil {
			retry("provider_binding_conflict", true)
			return
		} else if op.Kind == "delete" {
			// GET proves the resource still exists; DELETE's provider contract is
			// idempotent. Never issue another create or an opposite expiry mutation.
			if err = s.provider.DeleteCacheContent(ctx, account, resource.UpstreamName); err != nil {
				retry("delete_retry_pending", false)
				return
			}
		} else if !result.ExpiresAt.Equal(op.TargetExpiry) {
			if op.BaselineExpiry != nil && result.ExpiresAt.Equal(*op.BaselineExpiry) {
				// A GET at baseline cannot prove that a timed-out PATCH never
				// executes later. Preserve the reservation for reconciliation.
				retry("patch_baseline_result_unknown", true)
				return
			}
			retry("expiry_conflict", true)
			return
		}
	default:
		retry("unsupported_operation", true)
		return
	}
	if err = s.confirmAndSettle(ctx, op, result); err != nil {
		retry("confirmation_pending", false)
	}
}
