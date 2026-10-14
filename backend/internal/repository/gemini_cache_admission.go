package repository

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Financial generations have no TTL: an arbitrarily delayed old cache fill
// must not regain authority just because the ordinary cache entry expired.
const setGeminiFencedHashScript = `
local generation = redis.call('GET', KEYS[2]) or '0'
if tonumber(ARGV[1]) < tonumber(generation) then return 0 end
-- Preserve concurrent ordinary usage across financial generations. Explicit
-- resets atomically advance the fence and delete the old hash separately.
local preserveUsage = tonumber(ARGV[1]) > 0
if tonumber(ARGV[1]) > 0 then redis.call('SET', KEYS[2], ARGV[1]) end
local oldVersion = redis.call('HGET', KEYS[1], 'version')
local newVersion = nil
local windows = {}
local oldWindows = {}
local staleWindows = {}
for i=3,#ARGV,2 do
  if ARGV[i] == 'version' then newVersion = ARGV[i+1] end
  if string.sub(ARGV[i],1,7) == 'window_' or string.find(ARGV[i], '_window_start', 1, true) then
    windows[ARGV[i]] = ARGV[i+1]
    oldWindows[ARGV[i]] = redis.call('HGET',KEYS[1],ARGV[i])
  end
end
for window,value in pairs(windows) do
  if preserveUsage and tonumber(oldWindows[window] or '0') > (tonumber(value) or 0) then
    staleWindows[window] = true
  end
end
for i=3,#ARGV,2 do
  local field = ARGV[i]
  local value = ARGV[i+1]
  local old = redis.call('HGET', KEYS[1], field)
  local preserve = false
  local window = nil
  if field == 'daily_usage' or field == 'weekly_usage' or field == 'monthly_usage' then
    window = string.gsub(field, '_usage', '_window_start')
    if windows[window] ~= nil then
      preserve = preserveUsage and oldWindows[window] == windows[window]
    else
      preserve = preserveUsage and oldVersion and newVersion and tonumber(oldVersion) >= tonumber(newVersion)
    end
  elseif field == 'usage_5h' or field == 'usage_1d' or field == 'usage_7d' then
    window = 'window_' .. string.sub(field,7)
    preserve = preserveUsage and oldWindows[window] == windows[window]
  end
  if (staleWindows[field] or (window and staleWindows[window])) and old then
    value = old
  elseif preserve and old and tonumber(old) > tonumber(value) then value = old end
  redis.call('HSET', KEYS[1],field,value)
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`

func (c *billingCache) setGeminiFencedHash(ctx context.Context, key string, fields map[string]any, generation int64, ttl time.Duration) error {
	args := []any{generation, ttl.Milliseconds()}
	for field, value := range fields {
		args = append(args, field, value)
	}
	return c.rdb.Eval(ctx, setGeminiFencedHashScript, []string{key, key + ":gc-generation"}, args...).Err()
}

// Reset invalidation must fence old fills before deleting the hash. A late
// invalidation sampled before a newer financial commit cannot delete its data.
func (c *billingCache) invalidateGeminiFencedHash(ctx context.Context, key string, generation int64) error {
	const script = `
if tonumber(ARGV[1]) < tonumber(redis.call('GET',KEYS[2]) or '0') then return 0 end
if tonumber(ARGV[1]) > 0 then redis.call('SET',KEYS[2],ARGV[1]) end
redis.call('DEL',KEYS[1])
return 1`
	return c.rdb.Eval(ctx, script, []string{key, key + ":gc-generation"}, generation).Err()
}

func (c *billingCache) InvalidateSubscriptionCacheWithGeneration(ctx context.Context, userID, groupID, generation int64) error {
	return c.invalidateGeminiFencedHash(ctx, billingSubKey(userID, groupID), generation)
}

func (c *billingCache) InvalidateAPIKeyRateLimitWithGeneration(ctx context.Context, keyID, generation int64) error {
	return c.invalidateGeminiFencedHash(ctx, billingRateLimitKey(keyID), generation)
}

func (c *billingCache) SetUserBalanceWithGeneration(ctx context.Context, userID int64, balance float64, generation int64) error {
	const script = `
local generation = tonumber(redis.call('GET', KEYS[2]) or '0')
if tonumber(ARGV[1]) < generation then return 0 end
if tonumber(ARGV[1]) > 0 then redis.call('SET', KEYS[2], ARGV[1]) end
local value = tonumber(ARGV[2])
local old = tonumber(redis.call('GET', KEYS[1]))
if tonumber(ARGV[1]) > 0 and tonumber(ARGV[1]) == generation and old and old < value then value = old end
redis.call('SET', KEYS[1], value, 'PX', ARGV[3])
return 1`
	key := billingBalanceKey(userID)
	return c.rdb.Eval(ctx, script, []string{key, key + ":gc-generation"}, generation, strconv.FormatFloat(balance, 'f', -1, 64), jitteredTTL().Milliseconds()).Err()
}

type geminiCacheAdmissionPublisher struct {
	db      *sql.DB
	billing *billingCache
	keys    *apiKeyCache
}

func NewGeminiCacheAdmissionPublisher(db *sql.DB, cache service.BillingCache, keys service.APIKeyCache) service.GeminiCacheAdmissionPublisher {
	b, bok := cache.(*billingCache)
	k, kok := keys.(*apiKeyCache)
	if !bok || !kok {
		return nil
	}
	return &geminiCacheAdmissionPublisher{db: db, billing: b, keys: k}
}

// Called only after a reservation/capture/refund commit, before provider writes
// or publication of an active resource. Read the authoritative DB objects.
func (p *geminiCacheAdmissionPublisher) PublishCacheChargeAdmission(ctx context.Context, op *service.GeminiCacheOperation) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var balance float64
	var balanceGeneration int64
	if err := p.db.QueryRowContext(ctx, `SELECT balance,gemini_cache_generation FROM users WHERE id=$1`, op.Owner.UserID).Scan(&balance, &balanceGeneration); err != nil {
		return err
	}
	// Invalidate after fencing instead of overwriting a concurrent generation
	// debit with a DB snapshot sampled before that debit. Admission refills via
	// its existing miss path; old fills cannot cross the durable generation.
	const fenceBalance = `
if tonumber(ARGV[1]) < tonumber(redis.call('GET',KEYS[2]) or '0') then return 0 end
redis.call('SET',KEYS[2],ARGV[1])
redis.call('DEL',KEYS[1])
return 1`
	keyBalance := billingBalanceKey(op.Owner.UserID)
	if err := p.billing.rdb.Eval(ctx, fenceBalance, []string{keyBalance, keyBalance + ":gc-generation"}, balanceGeneration).Err(); err != nil {
		return err
	}
	var key string
	var keyGeneration int64
	if err := p.db.QueryRowContext(ctx, `SELECT key,gemini_cache_generation FROM api_keys WHERE id=$1`, op.Owner.APIKeyID).Scan(&key, &keyGeneration); err != nil {
		return err
	}
	if err := p.keys.FenceCacheChargeAuth(ctx, key, keyGeneration); err != nil {
		return err
	}
	var rate service.APIKeyRateLimitCacheData
	if err := p.db.QueryRowContext(ctx, `SELECT usage_5h,usage_1d,usage_7d,COALESCE(EXTRACT(EPOCH FROM window_5h_start)::bigint,0),COALESCE(EXTRACT(EPOCH FROM window_1d_start)::bigint,0),COALESCE(EXTRACT(EPOCH FROM window_7d_start)::bigint,0),gemini_cache_pending,gemini_cache_generation FROM api_keys WHERE id=$1`, op.Owner.APIKeyID).Scan(&rate.Usage5h, &rate.Usage1d, &rate.Usage7d, &rate.Window5h, &rate.Window1d, &rate.Window7d, &rate.GeminiCachePending, &rate.GeminiCacheGeneration); err != nil {
		return err
	}
	if err := p.billing.SetAPIKeyRateLimit(ctx, op.Owner.APIKeyID, &rate); err != nil {
		return err
	}
	if op.SubscriptionID != nil {
		var sub service.SubscriptionCacheData
		if err := p.db.QueryRowContext(ctx, `SELECT status,expires_at,daily_usage_usd,weekly_usage_usd,monthly_usage_usd,gemini_cache_pending,gemini_cache_generation,EXTRACT(EPOCH FROM updated_at)::bigint,daily_window_start,weekly_window_start,monthly_window_start FROM user_subscriptions WHERE id=$1`, *op.SubscriptionID).Scan(&sub.Status, &sub.ExpiresAt, &sub.DailyUsage, &sub.WeeklyUsage, &sub.MonthlyUsage, &sub.GeminiCachePending, &sub.GeminiCacheGeneration, &sub.Version, &sub.DailyWindowStart, &sub.WeeklyWindowStart, &sub.MonthlyWindowStart); err != nil {
			return err
		}
		return p.billing.SetSubscriptionCache(ctx, op.Owner.UserID, op.Owner.GroupID, &sub)
	}
	var entry service.UserPlatformQuotaCacheEntry
	if err := p.db.QueryRowContext(ctx, `SELECT gemini_cache_pending,gemini_cache_generation,gemini_cache_daily_usage,gemini_cache_weekly_usage,gemini_cache_monthly_usage,gemini_cache_daily_start,gemini_cache_weekly_start,gemini_cache_monthly_start FROM user_platform_quotas WHERE user_id=$1 AND platform='gemini' AND deleted_at IS NULL`, op.Owner.UserID).Scan(&entry.GeminiCachePending, &entry.GeminiCacheGeneration, &entry.GeminiCacheDailyUsage, &entry.GeminiCacheWeeklyUsage, &entry.GeminiCacheMonthlyUsage, &entry.GeminiCacheDailyStart, &entry.GeminiCacheWeeklyStart, &entry.GeminiCacheMonthlyStart); err != nil {
		return err
	}
	fields := map[string]any{"gemini_cache_pending": entry.GeminiCachePending, "gemini_cache_generation": entry.GeminiCacheGeneration}
	stamp := func(t *time.Time) int64 {
		if t == nil {
			return 0
		}
		return t.Unix()
	}
	fields["gemini_cache_daily_usage"] = entry.GeminiCacheDailyUsage
	fields["gemini_cache_weekly_usage"] = entry.GeminiCacheWeeklyUsage
	fields["gemini_cache_monthly_usage"] = entry.GeminiCacheMonthlyUsage
	fields["gemini_cache_daily_start"] = stamp(entry.GeminiCacheDailyStart)
	fields["gemini_cache_weekly_start"] = stamp(entry.GeminiCacheWeeklyStart)
	fields["gemini_cache_monthly_start"] = stamp(entry.GeminiCacheMonthlyStart)
	// A missing hash is rebuilt on the management path. The ordinary fields
	// come from DB only in this case; existing Redis generation usage is retained.
	platformKey := userPlatformQuotaCacheKey(op.Owner.UserID, "gemini")
	exists, err := p.billing.rdb.Exists(ctx, platformKey).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		rec, err := p.loadPlatformAdmission(ctx, op.Owner.UserID)
		if err != nil {
			return err
		}
		if err = p.billing.SetUserPlatformQuotaCache(ctx, op.Owner.UserID, "gemini", rec, time.Hour); err != nil {
			return err
		}
	}
	return p.billing.setGeminiFencedHash(ctx, platformKey, fields, entry.GeminiCacheGeneration, time.Hour)
}
func (p *geminiCacheAdmissionPublisher) loadPlatformAdmission(ctx context.Context, userID int64) (*service.UserPlatformQuotaCacheEntry, error) {
	var e service.UserPlatformQuotaCacheEntry
	e.SchemaVersion = service.UserPlatformQuotaCacheSchemaV1
	err := p.db.QueryRowContext(ctx, `SELECT daily_usage_usd,weekly_usage_usd,monthly_usage_usd,daily_limit_usd,weekly_limit_usd,monthly_limit_usd,daily_window_start,weekly_window_start,monthly_window_start,gemini_cache_pending,gemini_cache_generation,gemini_cache_daily_usage,gemini_cache_weekly_usage,gemini_cache_monthly_usage,gemini_cache_daily_start,gemini_cache_weekly_start,gemini_cache_monthly_start FROM user_platform_quotas WHERE user_id=$1 AND platform='gemini' AND deleted_at IS NULL`, userID).Scan(&e.DailyUsageUSD, &e.WeeklyUsageUSD, &e.MonthlyUsageUSD, &e.DailyLimitUSD, &e.WeeklyLimitUSD, &e.MonthlyLimitUSD, &e.DailyWindowStart, &e.WeeklyWindowStart, &e.MonthlyWindowStart, &e.GeminiCachePending, &e.GeminiCacheGeneration, &e.GeminiCacheDailyUsage, &e.GeminiCacheWeeklyUsage, &e.GeminiCacheMonthlyUsage, &e.GeminiCacheDailyStart, &e.GeminiCacheWeeklyStart, &e.GeminiCacheMonthlyStart)
	return &e, err
}
