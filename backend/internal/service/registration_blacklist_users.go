package service

import (
	"context"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"time"
)

type RegistrationBlacklistCandidate struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	UpdatedAt time.Time `json:"updated_at"`
}
type RegistrationBlacklistPreview struct {
	Revision string                           `json:"revision"`
	Users    []RegistrationBlacklistCandidate `json:"users"`
	HasMore  bool                             `json:"has_more"`
	NextID   int64                            `json:"next_id"`
}
type RegistrationBlacklistDisableRequest struct {
	Revision string                           `json:"revision"`
	BatchID  string                           `json:"batch_id"`
	Confirm  bool                             `json:"confirm"`
	Users    []RegistrationBlacklistCandidate `json:"users"`
}
type RegistrationBlacklistUserRepository interface {
	PreviewRegistrationBlacklist(context.Context, int64, int64) (*RegistrationBlacklistPreview, error)
	DisableRegistrationBlacklist(context.Context, int64, RegistrationBlacklistDisableRequest) ([]int64, error)
}

func (s *UserService) PreviewRegistrationBlacklist(ctx context.Context, actor, after int64) (*RegistrationBlacklistPreview, error) {
	repo, ok := s.userRepo.(RegistrationBlacklistUserRepository)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return repo.PreviewRegistrationBlacklist(ctx, actor, after)
}
func (s *UserService) DisableRegistrationBlacklist(ctx context.Context, actor int64, req RegistrationBlacklistDisableRequest) ([]int64, error) {
	if !req.Confirm || len(req.Users) == 0 || len(req.Users) > 100 {
		return nil, infraerrors.BadRequest("INVALID_BLACKLIST_BATCH", "Confirm 1–100 previewed users per batch")
	}
	repo, ok := s.userRepo.(RegistrationBlacklistUserRepository)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	ids, err := repo.DisableRegistrationBlacklist(ctx, actor, req)
	if err == nil && s.authCacheInvalidator != nil {
		for _, id := range ids {
			s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, id)
		}
	}
	return ids, err
}

// Shared with the transaction implementation; malformed stored rules fail closed.
func DecodeRegistrationEmailBlacklist(raw string) ([]string, error) {
	return decodeRegistrationBlacklist(raw)
}
