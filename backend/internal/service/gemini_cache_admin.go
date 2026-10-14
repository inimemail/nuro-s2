package service

import (
	"context"
	"time"
)

type GeminiCacheReconcileRow struct {
	OperationID     string `json:"operationId"`
	ResourceName    string `json:"resourceName"`
	UserID          int64  `json:"userId"`
	APIKeyID        int64  `json:"apiKeyId"`
	GroupID         int64  `json:"groupId"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
	Attempts        int    `json:"attempts"`
	ErrorCode       string `json:"errorCode"`
	ReservedAmount  string `json:"reservedAmount"`
	ConfirmedTokens *int64 `json:"confirmedTokens"`
	ActualAmount    string `json:"actualAmount"`
	DeficitAmount   string `json:"deficitAmount"`
}
type GeminiCacheAdminRepository interface {
	ListReconcile(context.Context, string, int) ([]GeminiCacheReconcileRow, string, error)
	RequeueReconcile(context.Context, string, int64, string) error
}

func (s *GeminiCachedContentService) ListReconcile(ctx context.Context, page string) ([]GeminiCacheReconcileRow, string, error) {
	r, ok := s.repo.(GeminiCacheAdminRepository)
	if !ok {
		return nil, "", ErrGeminiCacheUnavailable
	}
	return r.ListReconcile(ctx, page, 50)
}
func (s *GeminiCachedContentService) RequeueReconcile(ctx context.Context, id string, actor int64, reason string) error {
	r, ok := s.repo.(GeminiCacheAdminRepository)
	if !ok {
		return ErrGeminiCacheUnavailable
	}
	return r.RequeueReconcile(ctx, id, actor, reason)
}

func (s *GeminiCachedContentService) TopUpConfirmed(ctx context.Context, id string, actor int64, reason, expected string) error {
	r, ok := s.charges.(interface {
		TopUpConfirmedCacheCharge(context.Context, string, int64, string, string) error
	})
	if !ok {
		return ErrGeminiCacheUnavailable
	}
	return r.TopUpConfirmedCacheCharge(ctx, id, actor, reason, expected)
}

func (s *GeminiCachedContentService) OperationStatus(ctx context.Context, owner GeminiCacheOwner, kind, idempotency string) (string, string, error) {
	if kind != "create" && kind != "patch" && kind != "delete" {
		return "", "", ErrGeminiCacheNotFound
	}
	hash, err := GeminiCacheIdempotencyHash(idempotency)
	if err != nil || hash == "" {
		return "", "", ErrGeminiCacheNotFound
	}
	op, err := s.repo.FindOperation(ctx, owner, kind, hash)
	if err != nil {
		return "", "", err
	}
	if op == nil {
		return "not_recorded", "", nil
	}
	return op.State, "cachedContents/" + op.ResourceID, nil
}

func (s *GeminiCachedContentService) CancelUnrecordedOperation(ctx context.Context, owner GeminiCacheOwner, kind, idempotency string) error {
	if kind != "create" && kind != "patch" && kind != "delete" {
		return ErrGeminiCacheNotFound
	}
	hash, err := GeminiCacheIdempotencyHash(idempotency)
	if err != nil || hash == "" {
		return ErrGeminiCacheNotFound
	}
	r, ok := s.repo.(interface {
		CancelUnrecordedOperation(context.Context, GeminiCacheOwner, string, string) error
	})
	if !ok {
		return ErrGeminiCacheUnavailable
	}
	return r.CancelUnrecordedOperation(ctx, owner, kind, hash)
}
func (s *GeminiCachedContentService) AuthorizationRevision(ctx context.Context) (int64, error) {
	if !s.Enabled() {
		return 0, nil
	}
	r, ok := s.repo.(interface {
		CurrentViewRevision(context.Context) (int64, error)
	})
	if !ok {
		return 0, ErrGeminiCacheUnavailable
	}
	return r.CurrentViewRevision(ctx)
}
func (s *GeminiCachedContentService) WaitAuthorizationBarrier(ctx context.Context, before int64, waitAuthCache bool) error {
	if !s.Enabled() {
		return nil
	}
	r, ok := s.repo.(interface {
		AuthorizationRevisionAfter(context.Context, int64) (int64, error)
	})
	if !ok {
		return ErrGeminiCacheUnavailable
	}
	revision, err := r.AuthorizationRevisionAfter(ctx, before)
	if err != nil {
		return err
	}
	if revision <= before {
		return nil
	}
	started := time.Now()
	if err = WaitGeminiCacheBarrier(ctx, s.repo, revision); err != nil {
		return err
	}
	// Account eligibility is carried in the leased local resource view, not
	// API-key auth entries. Account switches only wait for that view's fence.
	if !waitAuthCache {
		return nil
	}
	remaining := s.authBarrier - time.Since(started)
	if remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
