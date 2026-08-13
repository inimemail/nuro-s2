package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const leaderLockKeyPrefix = "leader:lock:"

var leaderLockReleaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

type leaderLockCache struct{ rdb *redis.Client }

func NewLeaderLockCache(rdb *redis.Client) service.LeaderLockCache { return &leaderLockCache{rdb: rdb} }

func (c *leaderLockCache) TryAcquireLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	if c == nil || c.rdb == nil {
		return false, context.Canceled
	}
	return c.rdb.SetNX(ctx, leaderLockKeyPrefix+key, owner, ttl).Result()
}

func (c *leaderLockCache) ReleaseLeaderLock(ctx context.Context, key, owner string) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return leaderLockReleaseScript.Run(ctx, c.rdb, []string{leaderLockKeyPrefix + key}, owner).Err()
}
