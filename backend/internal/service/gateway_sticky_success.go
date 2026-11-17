package service

import (
	"context"
	"time"
)

type stickyBindingCAS interface {
	CompareAndSetSessionAccountID(context.Context, int64, string, int64, int64, time.Duration) (bool, error)
	CompareAndDeleteSessionAccountID(context.Context, int64, string, int64) (bool, error)
}

func (s *GatewayService) BindSuccessfulStickySession(ctx context.Context, groupID *int64, hash string, expected, accountID int64) error {
	if s == nil || s.cache == nil || hash == "" || accountID <= 0 {
		return nil
	}
	cache, ok := s.cache.(stickyBindingCAS)
	if !ok {
		return nil
	} // Do not degrade CAS to a racy SET.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	changed, err := cache.CompareAndSetSessionAccountID(writeCtx, derefGroupID(groupID), hash, expected, accountID, stickySessionTTL)
	if err != nil {
		return err
	}
	if changed {
		s.bindAnthropicCacheAffinitySessionForAccountID(writeCtx, groupID, accountID)
	}
	return nil
}
func (s *GatewayService) ReleaseFailedStickySession(ctx context.Context, groupID *int64, hash string, accountID int64) error {
	if s == nil || s.cache == nil || hash == "" || accountID <= 0 {
		return nil
	}
	cache, ok := s.cache.(stickyBindingCAS)
	if !ok {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_, err := cache.CompareAndDeleteSessionAccountID(writeCtx, derefGroupID(groupID), hash, accountID)
	return err
}

// Captured from the scheduler's existing read, without an additional cache query.
type stickyObservationKey struct{}
type stickyObservation struct {
	accountID int64
	observed  bool
}

// A Redis miss is a trustworthy observation of absence; transport failures are not.
func stickyReadObserved(cache GatewayCache, err error) bool {
	if err == nil {
		return true
	}
	miss, ok := cache.(interface{ IsSessionAccountNotFound(error) bool })
	return ok && miss.IsSessionAccountNotFound(err)
}
