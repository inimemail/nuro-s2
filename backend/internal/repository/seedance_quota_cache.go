package repository

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// An unacknowledged event has no TTL: a durable outbox can be retried after
// arbitrarily long outages. Acknowledged markers expire after the retry lease.
func (c *billingCache) ApplySeedancePlatformQuota(ctx context.Context, id string, userID int64, platform string, cost float64, ttl time.Duration) error {
	const script = `
if redis.call('EXISTS', KEYS[3]) == 1 then return 1 end
if redis.call('HGET', KEYS[1], 'schema_version') ~= ARGV[3] then return 0 end
local limited = false
for _, field in ipairs({'daily_limit','weekly_limit','monthly_limit'}) do
 local v=redis.call('HGET',KEYS[1],field)
 if v and v ~= '' then limited=true end
end
if limited then
 redis.call('HINCRBYFLOAT',KEYS[1],'daily_usage',ARGV[1])
 redis.call('HINCRBYFLOAT',KEYS[1],'weekly_usage',ARGV[1])
 redis.call('HINCRBYFLOAT',KEYS[1],'monthly_usage',ARGV[1])
 redis.call('HINCRBY',KEYS[1],'version',1)
 redis.call('EXPIRE',KEYS[1],ARGV[2])
 redis.call('SADD',KEYS[2],ARGV[4])
 redis.call('EXPIRE',KEYS[2],86400)
end
redis.call('SET',KEYS[3],'1')
return 1`
	n, err := c.rdb.Eval(ctx, script, []string{userPlatformQuotaCacheKey(userID, platform), userPlatformQuotaDirtySetKey(), "billing:seedance-quota:" + id}, strconv.FormatFloat(cost, 'f', -1, 64), max(1, int(ttl.Seconds())), service.UserPlatformQuotaCacheSchemaV1, userPlatformQuotaDirtyMember(userID, platform)).Int()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("Seedance quota cache not hydrated")
	}
	return nil
}

func (c *billingCache) AcknowledgeSeedancePlatformQuota(ctx context.Context, id string) error {
	return c.rdb.Expire(ctx, "billing:seedance-quota:"+id, 24*time.Hour).Err()
}
