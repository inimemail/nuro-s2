package service

import (
	"context"
	"database/sql"
	"time"
)

type LeaderLockCache interface {
	TryAcquireLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error)
	ReleaseLeaderLock(ctx context.Context, key, owner string) error
}

func tryAcquireSingletonLeaderLock(ctx context.Context, cache LeaderLockCache, db *sql.DB, key, owner string, ttl time.Duration) (func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cache != nil {
		ok, err := cache.TryAcquireLeaderLock(ctx, key, owner, ttl)
		if err == nil {
			if !ok {
				return nil, false
			}
			return func() {
				ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = cache.ReleaseLeaderLock(ctx2, key, owner)
			}, true
		}
	}
	if db != nil {
		return tryAcquireDBAdvisoryLock(ctx, db, hashAdvisoryLockID(key))
	}
	return func() {}, true
}
