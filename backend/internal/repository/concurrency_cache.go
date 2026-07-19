package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/runtimeops"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// 并发控制缓存常量定义
//
// 性能优化说明：
// 原实现使用 SCAN 命令遍历独立的槽位键（concurrency:account:{id}:{requestID}），
// 在高并发场景下 SCAN 需要多次往返，且遍历大量键时性能下降明显。
//
// 新实现改用 Redis 有序集合（Sorted Set）：
// 1. 每个账号/用户只有一个键，成员为 requestID，分数为时间戳
// 2. 使用 ZCARD 原子获取并发数，时间复杂度 O(1)
// 3. 使用 ZREMRANGEBYSCORE 清理过期槽位，避免手动管理 TTL
// 4. 单次 Redis 调用完成计数，减少网络往返
const (
	// 并发槽位键前缀（有序集合）
	// 格式: concurrency:account:{accountID}
	accountSlotKeyPrefix = "concurrency:account:"
	// 格式: concurrency:user:{userID}
	userSlotKeyPrefix = "concurrency:user:"
	// 格式: concurrency:api_key:{apiKeyID}
	apiKeySlotKeyPrefix = "concurrency:api_key:"
	// 等待队列计数器格式: concurrency:wait:{userID}
	waitQueueKeyPrefix = "concurrency:wait:"
	// 账号级等待队列计数器格式: wait:account:{accountID}
	accountWaitKeyPrefix = "wait:account:"
	// OpenAI 账号软冷却/运行时阻断键格式: cooldown:account:{accountID}
	accountCooldownKeyPrefix = "cooldown:account:"

	// 默认槽位过期时间（分钟），可通过配置覆盖
	defaultSlotTTLMinutes = 15
)

var (
	// acquireScript 使用有序集合计数并在未达上限时添加槽位
	// 使用 Redis TIME 命令获取服务器时间，避免多实例时钟不同步问题
	// KEYS[1] = 有序集合键 (concurrency:account:{id} / concurrency:user:{id})
	// ARGV[1] = maxConcurrency
	// ARGV[2] = TTL（秒）
	// ARGV[3] = requestID
	acquireScript = redis.NewScript(`
		local key = KEYS[1]
		local maxConcurrency = tonumber(ARGV[1])
		local ttl = tonumber(ARGV[2])
		local requestID = ARGV[3]

		-- 使用 Redis 服务器时间，确保多实例时钟一致
		local timeResult = redis.call('TIME')
		local now = tonumber(timeResult[1])
		local expireBefore = now - ttl

		-- 清理过期槽位
		redis.call('ZREMRANGEBYSCORE', key, '-inf', expireBefore)

		-- 检查是否已存在（支持重试场景刷新时间戳）
		local exists = redis.call('ZSCORE', key, requestID)
		if exists ~= false then
			redis.call('ZADD', key, now, requestID)
			redis.call('EXPIRE', key, ttl)
			return 1
		end

		-- 检查是否达到并发上限
		local count = redis.call('ZCARD', key)
		if count < maxConcurrency then
			redis.call('ZADD', key, now, requestID)
			redis.call('EXPIRE', key, ttl)
			return 1
		end

		return 0
	`)

	// acquireFirstAvailableAccountSlotScript 按调度器给出的候选顺序，在一次
	// Redis 往返里抢占第一个有可用容量的账号槽位。
	// KEYS pairs = concurrency:account:{accountID}, cooldown:account:{accountID}
	// ARGV[1] = TTL（秒）
	// ARGV[2] = requestID
	// ARGV[3...] = 每个 KEYS 对应的 maxConcurrency
	acquireFirstAvailableAccountSlotScript = redis.NewScript(`
		local ttl = tonumber(ARGV[1])
		local requestID = ARGV[2]

		local timeResult = redis.call('TIME')
		local now = tonumber(timeResult[1])
		local expireBefore = now - ttl

		for i = 1, #KEYS, 2 do
			local key = KEYS[i]
			local cooldownKey = KEYS[i + 1]
			local candidateIndex = ((i - 1) / 2) + 1
			local maxConcurrency = tonumber(ARGV[candidateIndex + 2])
			if maxConcurrency ~= nil and maxConcurrency > 0 and redis.call('EXISTS', cooldownKey) == 0 then
				local exists = redis.call('ZSCORE', key, requestID)
				if exists ~= false and tonumber(exists) > expireBefore then
					redis.call('ZADD', key, now, requestID)
					redis.call('EXPIRE', key, ttl)
					return candidateIndex
				elseif exists ~= false then
					redis.call('ZREM', key, requestID)
				end

				local count = redis.call('ZCOUNT', key, '(' .. expireBefore, '+inf')
				if count < maxConcurrency then
					redis.call('ZADD', key, now, requestID)
					redis.call('EXPIRE', key, ttl)
					return candidateIndex
				end
			end
		end

		return 0
	`)

	acquireFirstAvailableUserAccountSlotsScript = redis.NewScript(`
		local userKey = KEYS[1]
		local ttl = tonumber(ARGV[1])
		local userMaxConcurrency = tonumber(ARGV[2])
		local userRequestID = ARGV[3]
		local accountRequestID = ARGV[4]

		local timeResult = redis.call('TIME')
		local now = tonumber(timeResult[1])
		local expireBefore = now - ttl

		local userExists = redis.call('ZSCORE', userKey, userRequestID)
		if userExists ~= false and tonumber(userExists) > expireBefore then
			redis.call('ZADD', userKey, now, userRequestID)
			redis.call('EXPIRE', userKey, ttl)
		else
			if userExists ~= false then
				redis.call('ZREM', userKey, userRequestID)
			end
			local userTotalCount = redis.call('ZCARD', userKey)
			if userTotalCount > (userMaxConcurrency * 2) then
				redis.call('ZREMRANGEBYSCORE', userKey, '-inf', expireBefore)
			end
			local userCount = redis.call('ZCOUNT', userKey, '(' .. expireBefore, '+inf')
			if userCount >= userMaxConcurrency then
				redis.call('ZREMRANGEBYSCORE', userKey, '-inf', expireBefore)
				if redis.call('ZCARD', userKey) >= userMaxConcurrency then
					return 0
				end
			end
			redis.call('ZADD', userKey, now, userRequestID)
			redis.call('EXPIRE', userKey, ttl)
		end

		for i = 2, #KEYS, 2 do
			local slotKey = KEYS[i]
			local cooldownKey = KEYS[i + 1]
			local candidateIndex = ((i - 2) / 2) + 1
			local maxConcurrency = tonumber(ARGV[candidateIndex + 4])
			if maxConcurrency ~= nil and maxConcurrency > 0 and redis.call('EXISTS', cooldownKey) == 0 then
				local accountExists = redis.call('ZSCORE', slotKey, accountRequestID)
				if accountExists ~= false and tonumber(accountExists) > expireBefore then
					redis.call('ZADD', slotKey, now, accountRequestID)
					redis.call('EXPIRE', slotKey, ttl)
					return candidateIndex
				elseif accountExists ~= false then
					redis.call('ZREM', slotKey, accountRequestID)
				end

				local count = redis.call('ZCOUNT', slotKey, '(' .. expireBefore, '+inf')
				if count < maxConcurrency then
					redis.call('ZADD', slotKey, now, accountRequestID)
					redis.call('EXPIRE', slotKey, ttl)
					return candidateIndex
				end
			end
		end

		redis.call('ZREM', userKey, userRequestID)
			return 0
		`)

	// cleanupExpiredSlotKeysScript 批量清理实际存在的账号槽位键，避免后台任务从数据库加载全量账号。
	// KEYS = 有序集合键列表，ARGV[1] = TTL（秒）。
	cleanupExpiredSlotKeysScript = redis.NewScript(`
		-- Redis 3.2-4.x compat: opt into effects replication so redis.call('TIME')
		-- replicates correctly. No-op on Redis 5.0+.
		redis.replicate_commands()
		local ttl = tonumber(ARGV[1])
		local timeResult = redis.call('TIME')
		local now = tonumber(timeResult[1])
		local expireBefore = now - ttl
		local removed = 0
		for i = 1, #KEYS do
			local key = KEYS[i]
			removed = removed + redis.call('ZREMRANGEBYSCORE', key, '-inf', expireBefore)
			if redis.call('ZCARD', key) == 0 then
				redis.call('DEL', key)
			else
				redis.call('EXPIRE', key, ttl)
			end
		end
		return removed
	`)

	// getCountScript 统计有序集合中的槽位数量并清理过期条目
	// 使用 Redis TIME 命令获取服务器时间
	// KEYS[1] = 有序集合键
	// ARGV[1] = TTL（秒）
	getCountScript = redis.NewScript(`
		local key = KEYS[1]
		local ttl = tonumber(ARGV[1])

		-- 使用 Redis 服务器时间
		local timeResult = redis.call('TIME')
		local now = tonumber(timeResult[1])
		local expireBefore = now - ttl

		redis.call('ZREMRANGEBYSCORE', key, '-inf', expireBefore)
		return redis.call('ZCARD', key)
	`)

	// trackSlotScript 记录 stats-only 槽位，不做并发上限判断。
	// KEYS[1] = 有序集合键，ARGV[1] = TTL（秒），ARGV[2] = requestID。
	trackSlotScript = redis.NewScript(`
		local key = KEYS[1]
		local ttl = tonumber(ARGV[1])
		local requestID = ARGV[2]

		local timeResult = redis.call('TIME')
		local now = tonumber(timeResult[1])
		local expireBefore = now - ttl

		redis.call('ZREMRANGEBYSCORE', key, '-inf', expireBefore)
		redis.call('ZADD', key, now, requestID)
		redis.call('EXPIRE', key, ttl)
		return 1
	`)

	refreshSlotScript = redis.NewScript(`
		local key = KEYS[1]
		local requestID = ARGV[1]
		local ttl = tonumber(ARGV[2])
		if redis.call('ZSCORE', key, requestID) == false then
			return 0
		end
		local timeResult = redis.call('TIME')
		redis.call('ZADD', key, tonumber(timeResult[1]), requestID)
		redis.call('EXPIRE', key, ttl)
		return 1
	`)

	refreshSlotsScript = redis.NewScript(`
		local ttl = tonumber(ARGV[1])
		local timeResult = redis.call('TIME')
		local now = tonumber(timeResult[1])
		local refreshed = 0
		for i = 1, #KEYS do
			local requestID = ARGV[i + 1]
			if redis.call('ZSCORE', KEYS[i], requestID) ~= false then
				redis.call('ZADD', KEYS[i], now, requestID)
				redis.call('EXPIRE', KEYS[i], ttl)
				refreshed = refreshed + 1
			end
		end
		return refreshed
	`)

	// incrementWaitScript - refreshes TTL on each increment to keep queue depth accurate
	// KEYS[1] = wait queue key
	// ARGV[1] = maxWait
	// ARGV[2] = TTL in seconds
	incrementWaitScript = redis.NewScript(`
		local current = redis.call('GET', KEYS[1])
		if current == false then
			current = 0
		else
			current = tonumber(current)
		end

		if current >= tonumber(ARGV[1]) then
			return 0
		end

		local newVal = redis.call('INCR', KEYS[1])

		-- Refresh TTL so long-running traffic doesn't expire active queue counters.
		redis.call('EXPIRE', KEYS[1], ARGV[2])

			return 1
		`)

	// incrementAccountWaitScript - account-level wait queue count (refresh TTL on each increment)
	incrementAccountWaitScript = redis.NewScript(`
			local current = redis.call('GET', KEYS[1])
			if current == false then
				current = 0
			else
				current = tonumber(current)
			end

			if current >= tonumber(ARGV[1]) then
				return 0
			end

			local newVal = redis.call('INCR', KEYS[1])

			-- Refresh TTL so long-running traffic doesn't expire active queue counters.
			redis.call('EXPIRE', KEYS[1], ARGV[2])

			return 1
		`)

	// decrementWaitScript - same as before
	decrementWaitScript = redis.NewScript(`
			local current = redis.call('GET', KEYS[1])
			if current ~= false and tonumber(current) > 0 then
				redis.call('DECR', KEYS[1])
			end
			return 1
		`)

	// cleanupExpiredSlotsScript 清理单个账号/用户有序集合中过期槽位
	// KEYS[1] = 有序集合键
	// ARGV[1] = TTL（秒）
	cleanupExpiredSlotsScript = redis.NewScript(`
		local key = KEYS[1]
		local ttl = tonumber(ARGV[1])
		local timeResult = redis.call('TIME')
		local now = tonumber(timeResult[1])
		local expireBefore = now - ttl
		redis.call('ZREMRANGEBYSCORE', key, '-inf', expireBefore)
		if redis.call('ZCARD', key) == 0 then
			redis.call('DEL', key)
		else
			redis.call('EXPIRE', key, ttl)
		end
		return 1
	`)

	// startupCleanupScript 清理非当前进程前缀的槽位成员。
	// KEYS 是有序集合键列表，ARGV[1] 是当前进程前缀，ARGV[2] 是槽位 TTL。
	// 遍历每个 KEYS[i]，移除前缀不匹配的成员，清空后删 key，否则刷新 EXPIRE。
	startupCleanupScript = redis.NewScript(`
		local activePrefix = ARGV[1]
		local slotTTL = tonumber(ARGV[2])
		local removed = 0
		for i = 1, #KEYS do
			local key = KEYS[i]
			local members = redis.call('ZRANGE', key, 0, -1)
			for _, member in ipairs(members) do
				if string.sub(member, 1, string.len(activePrefix)) ~= activePrefix then
					removed = removed + redis.call('ZREM', key, member)
				end
			end
			if redis.call('ZCARD', key) == 0 then
				redis.call('DEL', key)
			else
				redis.call('EXPIRE', key, slotTTL)
			end
		end
		return removed
	`)
)

type concurrencyCache struct {
	rdb                 *redis.Client
	slotTTLSeconds      int // 槽位过期时间（秒）
	waitQueueTTLSeconds int // 等待队列过期时间（秒）
}

func (c *concurrencyCache) SlotTTL() time.Duration {
	if c == nil || c.slotTTLSeconds <= 0 {
		return 0
	}
	return time.Duration(c.slotTTLSeconds) * time.Second
}

// NewConcurrencyCache 创建并发控制缓存
// slotTTLMinutes: 槽位过期时间（分钟），0 或负数使用默认值 15 分钟
// waitQueueTTLSeconds: 等待队列过期时间（秒），0 或负数使用 slot TTL
func NewConcurrencyCache(rdb *redis.Client, slotTTLMinutes int, waitQueueTTLSeconds int) service.ConcurrencyCache {
	if slotTTLMinutes <= 0 {
		slotTTLMinutes = defaultSlotTTLMinutes
	}
	if waitQueueTTLSeconds <= 0 {
		waitQueueTTLSeconds = slotTTLMinutes * 60
	}
	return &concurrencyCache{
		rdb:                 rdb,
		slotTTLSeconds:      slotTTLMinutes * 60,
		waitQueueTTLSeconds: waitQueueTTLSeconds,
	}
}

// Helper functions for key generation
func accountSlotKey(accountID int64) string {
	return fmt.Sprintf("%s%d", accountSlotKeyPrefix, accountID)
}

func userSlotKey(userID int64) string {
	return fmt.Sprintf("%s%d", userSlotKeyPrefix, userID)
}

func apiKeySlotKey(apiKeyID int64) string {
	return fmt.Sprintf("%s%d", apiKeySlotKeyPrefix, apiKeyID)
}

func waitQueueKey(userID int64) string {
	return fmt.Sprintf("%s%d", waitQueueKeyPrefix, userID)
}

func accountWaitKey(accountID int64) string {
	return fmt.Sprintf("%s%d", accountWaitKeyPrefix, accountID)
}

func accountCooldownKey(accountID int64) string {
	return fmt.Sprintf("%s%d", accountCooldownKeyPrefix, accountID)
}

// Account slot operations

func (c *concurrencyCache) AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
	keys := []string{accountSlotKey(accountID), accountCooldownKey(accountID)}
	started := time.Now()
	// The cooldown key is checked in the same atomic claim that adds the slot,
	// closing the bounded snapshot-staleness window without another round trip.
	result, err := acquireFirstAvailableAccountSlotScript.Run(ctx, c.rdb, keys, c.slotTTLSeconds, requestID, maxConcurrency).Int()
	runtimeops.ObserveAdmissionClaim(time.Since(started), err)
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (c *concurrencyCache) AcquireFirstAvailableAccountSlot(ctx context.Context, candidates []service.AccountSlotCandidate, requestID string) (int64, bool, error) {
	if len(candidates) == 0 {
		return 0, false, nil
	}

	keys := make([]string, 0, len(candidates))
	args := make([]any, 0, len(candidates)+2)
	args = append(args, c.slotTTLSeconds, requestID)

	accountIDs := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.AccountID <= 0 || candidate.MaxConcurrency <= 0 {
			continue
		}
		keys = append(keys, accountSlotKey(candidate.AccountID), accountCooldownKey(candidate.AccountID))
		args = append(args, candidate.MaxConcurrency)
		accountIDs = append(accountIDs, candidate.AccountID)
	}
	if len(keys) == 0 {
		return 0, false, nil
	}

	started := time.Now()
	result, err := acquireFirstAvailableAccountSlotScript.Run(ctx, c.rdb, keys, args...).Int()
	runtimeops.ObserveAdmissionClaim(time.Since(started), err)
	if err != nil {
		return 0, false, err
	}
	if result <= 0 || result > len(accountIDs) {
		return 0, false, nil
	}
	return accountIDs[result-1], true, nil
}

func (c *concurrencyCache) AcquireFirstAvailableUserAccountSlots(ctx context.Context, userID int64, userMaxConcurrency int, candidates []service.AccountSlotCandidate, userRequestID string, accountRequestID string) (int64, bool, error) {
	if userID <= 0 || userMaxConcurrency <= 0 || len(candidates) == 0 {
		return 0, false, nil
	}

	keys := make([]string, 0, 1+len(candidates)*2)
	keys = append(keys, userSlotKey(userID))
	args := make([]any, 0, len(candidates)+4)
	args = append(args, c.slotTTLSeconds, userMaxConcurrency, userRequestID, accountRequestID)

	accountIDs := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.AccountID <= 0 || candidate.MaxConcurrency <= 0 {
			continue
		}
		keys = append(keys, accountSlotKey(candidate.AccountID), accountCooldownKey(candidate.AccountID))
		args = append(args, candidate.MaxConcurrency)
		accountIDs = append(accountIDs, candidate.AccountID)
	}
	if len(accountIDs) == 0 {
		return 0, false, nil
	}

	started := time.Now()
	result, err := acquireFirstAvailableUserAccountSlotsScript.Run(ctx, c.rdb, keys, args...).Int()
	runtimeops.ObserveAdmissionClaim(time.Since(started), err)
	if err != nil {
		return 0, false, err
	}
	if result <= 0 || result > len(accountIDs) {
		return 0, false, nil
	}
	return accountIDs[result-1], true, nil
}

func (c *concurrencyCache) ReleaseAccountSlot(ctx context.Context, accountID int64, requestID string) error {
	key := accountSlotKey(accountID)
	return c.rdb.ZRem(ctx, key, requestID).Err()
}

func (c *concurrencyCache) RefreshSlot(ctx context.Context, kind string, entityID int64, requestID string) error {
	key, err := concurrencySlotKey(kind, entityID)
	if err != nil {
		return err
	}
	_, err = refreshSlotScript.Run(ctx, c.rdb, []string{key}, requestID, c.slotTTLSeconds).Result()
	return err
}

func (c *concurrencyCache) RefreshSlots(ctx context.Context, renewals []service.ConcurrencySlotRenewal) error {
	const batchSize = 512
	for start := 0; start < len(renewals); start += batchSize {
		end := min(start+batchSize, len(renewals))
		keys := make([]string, 0, end-start)
		args := make([]any, 1, end-start+1)
		args[0] = c.slotTTLSeconds
		for _, renewal := range renewals[start:end] {
			key, err := concurrencySlotKey(renewal.Kind, renewal.EntityID)
			if err != nil || renewal.RequestID == "" {
				continue
			}
			keys = append(keys, key)
			args = append(args, renewal.RequestID)
		}
		if len(keys) == 0 {
			continue
		}
		if _, err := refreshSlotsScript.Run(ctx, c.rdb, keys, args...).Result(); err != nil {
			return err
		}
	}
	return nil
}

func concurrencySlotKey(kind string, entityID int64) (string, error) {
	switch kind {
	case "account":
		return accountSlotKey(entityID), nil
	case "user":
		return userSlotKey(entityID), nil
	case "api_key":
		return apiKeySlotKey(entityID), nil
	default:
		return "", fmt.Errorf("unsupported concurrency slot kind %q", kind)
	}
}

func (c *concurrencyCache) GetAccountConcurrency(ctx context.Context, accountID int64) (int, error) {
	key := accountSlotKey(accountID)
	// 时间戳在 Lua 脚本内使用 Redis TIME 命令获取
	result, err := getCountScript.Run(ctx, c.rdb, []string{key}, c.slotTTLSeconds).Int()
	if err != nil {
		return 0, err
	}
	return result, nil
}

func (c *concurrencyCache) GetAccountConcurrencyBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error) {
	if len(accountIDs) == 0 {
		return map[int64]int{}, nil
	}

	now, err := c.rdb.Time(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("redis TIME: %w", err)
	}
	cutoffTime := now.Unix() - int64(c.slotTTLSeconds)

	pipe := c.rdb.Pipeline()
	type accountCmd struct {
		accountID int64
		zcardCmd  *redis.IntCmd
	}
	cmds := make([]accountCmd, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		slotKey := accountSlotKeyPrefix + strconv.FormatInt(accountID, 10)
		pipe.ZRemRangeByScore(ctx, slotKey, "-inf", strconv.FormatInt(cutoffTime, 10))
		cmds = append(cmds, accountCmd{
			accountID: accountID,
			zcardCmd:  pipe.ZCard(ctx, slotKey),
		})
	}

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("pipeline exec: %w", err)
	}

	result := make(map[int64]int, len(accountIDs))
	for _, cmd := range cmds {
		result[cmd.accountID] = int(cmd.zcardCmd.Val())
	}
	return result, nil
}

// User slot operations

func (c *concurrencyCache) AcquireUserSlot(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
	key := userSlotKey(userID)
	started := time.Now()
	// 时间戳在 Lua 脚本内使用 Redis TIME 命令获取，确保多实例时钟一致
	result, err := acquireScript.Run(ctx, c.rdb, []string{key}, maxConcurrency, c.slotTTLSeconds, requestID).Int()
	runtimeops.ObserveAdmissionClaim(time.Since(started), err)
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (c *concurrencyCache) ReleaseUserSlot(ctx context.Context, userID int64, requestID string) error {
	key := userSlotKey(userID)
	return c.rdb.ZRem(ctx, key, requestID).Err()
}

func (c *concurrencyCache) GetUserConcurrency(ctx context.Context, userID int64) (int, error) {
	key := userSlotKey(userID)
	// 时间戳在 Lua 脚本内使用 Redis TIME 命令获取
	result, err := getCountScript.Run(ctx, c.rdb, []string{key}, c.slotTTLSeconds).Int()
	if err != nil {
		return 0, err
	}
	return result, nil
}

func (c *concurrencyCache) TrackAPIKeySlot(ctx context.Context, apiKeyID int64, requestID string) error {
	key := apiKeySlotKey(apiKeyID)
	_, err := trackSlotScript.Run(ctx, c.rdb, []string{key}, c.slotTTLSeconds, requestID).Result()
	return err
}

func (c *concurrencyCache) ReleaseAPIKeySlot(ctx context.Context, apiKeyID int64, requestID string) error {
	key := apiKeySlotKey(apiKeyID)
	return c.rdb.ZRem(ctx, key, requestID).Err()
}

func (c *concurrencyCache) GetAPIKeyConcurrencyBatch(ctx context.Context, apiKeyIDs []int64) (map[int64]int, error) {
	if len(apiKeyIDs) == 0 {
		return map[int64]int{}, nil
	}

	now, err := c.rdb.Time(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("redis TIME: %w", err)
	}
	cutoffTime := now.Unix() - int64(c.slotTTLSeconds)

	pipe := c.rdb.Pipeline()
	type apiKeyCmd struct {
		apiKeyID int64
		zcardCmd *redis.IntCmd
	}
	cmds := make([]apiKeyCmd, 0, len(apiKeyIDs))
	for _, apiKeyID := range apiKeyIDs {
		slotKey := apiKeySlotKeyPrefix + strconv.FormatInt(apiKeyID, 10)
		pipe.ZRemRangeByScore(ctx, slotKey, "-inf", strconv.FormatInt(cutoffTime, 10))
		cmds = append(cmds, apiKeyCmd{
			apiKeyID: apiKeyID,
			zcardCmd: pipe.ZCard(ctx, slotKey),
		})
	}

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("pipeline exec: %w", err)
	}

	result := make(map[int64]int, len(apiKeyIDs))
	for _, cmd := range cmds {
		result[cmd.apiKeyID] = int(cmd.zcardCmd.Val())
	}
	return result, nil
}

// Wait queue operations

func (c *concurrencyCache) IncrementWaitCount(ctx context.Context, userID int64, maxWait int) (bool, error) {
	key := waitQueueKey(userID)
	result, err := incrementWaitScript.Run(ctx, c.rdb, []string{key}, maxWait, c.waitQueueTTLSeconds).Int()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (c *concurrencyCache) DecrementWaitCount(ctx context.Context, userID int64) error {
	key := waitQueueKey(userID)
	_, err := decrementWaitScript.Run(ctx, c.rdb, []string{key}).Result()
	return err
}

// Account wait queue operations

func (c *concurrencyCache) IncrementAccountWaitCount(ctx context.Context, accountID int64, maxWait int) (bool, error) {
	key := accountWaitKey(accountID)
	result, err := incrementAccountWaitScript.Run(ctx, c.rdb, []string{key}, maxWait, c.waitQueueTTLSeconds).Int()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (c *concurrencyCache) DecrementAccountWaitCount(ctx context.Context, accountID int64) error {
	key := accountWaitKey(accountID)
	_, err := decrementWaitScript.Run(ctx, c.rdb, []string{key}).Result()
	return err
}

func (c *concurrencyCache) GetAccountWaitingCount(ctx context.Context, accountID int64) (int, error) {
	key := accountWaitKey(accountID)
	val, err := c.rdb.Get(ctx, key).Int()
	if err != nil && !errors.Is(err, redis.Nil) {
		return 0, err
	}
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return val, nil
}

func (c *concurrencyCache) SetAccountCooldown(ctx context.Context, accountID int64, ttl time.Duration) error {
	if accountID <= 0 || ttl <= 0 {
		return nil
	}
	return c.rdb.Set(ctx, accountCooldownKey(accountID), "1", ttl).Err()
}

func (c *concurrencyCache) ClearAccountCooldown(ctx context.Context, accountID int64) error {
	if accountID <= 0 {
		return nil
	}
	return c.rdb.Del(ctx, accountCooldownKey(accountID)).Err()
}

func (c *concurrencyCache) GetAccountsLoadBatch(ctx context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	if len(accounts) == 0 {
		return map[int64]*service.AccountLoadInfo{}, nil
	}

	// 使用 Pipeline 替代 Lua 脚本，兼容 Redis Cluster（Lua 内动态拼 key 会 CROSSSLOT）。
	// 每个账号执行 3 个命令：ZREMRANGEBYSCORE（清理过期）、ZCARD（并发数）、GET（等待数）。
	now, err := c.rdb.Time(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("redis TIME: %w", err)
	}
	cutoffTime := now.Unix() - int64(c.slotTTLSeconds)

	pipe := c.rdb.Pipeline()

	type accountCmds struct {
		id             int64
		maxConcurrency int
		zcardCmd       *redis.IntCmd
		getCmd         *redis.StringCmd
	}
	cmds := make([]accountCmds, 0, len(accounts))
	for _, acc := range accounts {
		slotKey := accountSlotKeyPrefix + strconv.FormatInt(acc.ID, 10)
		waitKey := accountWaitKeyPrefix + strconv.FormatInt(acc.ID, 10)
		pipe.ZRemRangeByScore(ctx, slotKey, "-inf", strconv.FormatInt(cutoffTime, 10))
		ac := accountCmds{
			id:             acc.ID,
			maxConcurrency: acc.MaxConcurrency,
			zcardCmd:       pipe.ZCard(ctx, slotKey),
			getCmd:         pipe.Get(ctx, waitKey),
		}
		cmds = append(cmds, ac)
	}

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("pipeline exec: %w", err)
	}

	loadMap := make(map[int64]*service.AccountLoadInfo, len(accounts))
	for _, ac := range cmds {
		currentConcurrency := int(ac.zcardCmd.Val())
		waitingCount := 0
		if v, err := ac.getCmd.Int(); err == nil {
			waitingCount = v
		}
		loadRate := 0
		if ac.maxConcurrency > 0 {
			loadRate = (currentConcurrency + waitingCount) * 100 / ac.maxConcurrency
		}
		loadMap[ac.id] = &service.AccountLoadInfo{
			AccountID:          ac.id,
			CurrentConcurrency: currentConcurrency,
			WaitingCount:       waitingCount,
			LoadRate:           loadRate,
		}
	}

	return loadMap, nil
}

func (c *concurrencyCache) GetUsersLoadBatch(ctx context.Context, users []service.UserWithConcurrency) (map[int64]*service.UserLoadInfo, error) {
	if len(users) == 0 {
		return map[int64]*service.UserLoadInfo{}, nil
	}

	// 使用 Pipeline 替代 Lua 脚本，兼容 Redis Cluster。
	now, err := c.rdb.Time(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("redis TIME: %w", err)
	}
	cutoffTime := now.Unix() - int64(c.slotTTLSeconds)

	pipe := c.rdb.Pipeline()

	type userCmds struct {
		id             int64
		maxConcurrency int
		zcardCmd       *redis.IntCmd
		getCmd         *redis.StringCmd
	}
	cmds := make([]userCmds, 0, len(users))
	for _, u := range users {
		slotKey := userSlotKeyPrefix + strconv.FormatInt(u.ID, 10)
		waitKey := waitQueueKeyPrefix + strconv.FormatInt(u.ID, 10)
		pipe.ZRemRangeByScore(ctx, slotKey, "-inf", strconv.FormatInt(cutoffTime, 10))
		uc := userCmds{
			id:             u.ID,
			maxConcurrency: u.MaxConcurrency,
			zcardCmd:       pipe.ZCard(ctx, slotKey),
			getCmd:         pipe.Get(ctx, waitKey),
		}
		cmds = append(cmds, uc)
	}

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("pipeline exec: %w", err)
	}

	loadMap := make(map[int64]*service.UserLoadInfo, len(users))
	for _, uc := range cmds {
		currentConcurrency := int(uc.zcardCmd.Val())
		waitingCount := 0
		if v, err := uc.getCmd.Int(); err == nil {
			waitingCount = v
		}
		loadRate := 0
		if uc.maxConcurrency > 0 {
			loadRate = (currentConcurrency + waitingCount) * 100 / uc.maxConcurrency
		}
		loadMap[uc.id] = &service.UserLoadInfo{
			UserID:             uc.id,
			CurrentConcurrency: currentConcurrency,
			WaitingCount:       waitingCount,
			LoadRate:           loadRate,
		}
	}

	return loadMap, nil
}

func (c *concurrencyCache) CleanupExpiredAccountSlots(ctx context.Context, accountID int64) error {
	key := accountSlotKey(accountID)
	_, err := cleanupExpiredSlotsScript.Run(ctx, c.rdb, []string{key}, c.slotTTLSeconds).Result()
	return err
}

func (c *concurrencyCache) CleanupExpiredAccountSlotKeys(ctx context.Context) error {
	return c.cleanupExpiredSlotKeysByPattern(ctx, accountSlotKeyPrefix+"*")
}

func (c *concurrencyCache) CleanupStaleProcessSlots(ctx context.Context, activeRequestPrefix string) error {
	if activeRequestPrefix == "" {
		return nil
	}

	// 1. 清理有序集合中非当前进程前缀的成员
	slotPatterns := []string{accountSlotKeyPrefix + "*", userSlotKeyPrefix + "*", apiKeySlotKeyPrefix + "*"}
	for _, pattern := range slotPatterns {
		if err := c.cleanupSlotsByPattern(ctx, pattern, activeRequestPrefix); err != nil {
			return err
		}
	}

	// 2. 删除所有等待队列计数器（重启后计数器失效）
	waitPatterns := []string{accountWaitKeyPrefix + "*", waitQueueKeyPrefix + "*"}
	for _, pattern := range waitPatterns {
		if err := c.deleteKeysByPattern(ctx, pattern); err != nil {
			return err
		}
	}

	return nil
}

// cleanupExpiredSlotKeysByPattern 扫描实际存在的账号槽位键并批量清理过期成员。
func (c *concurrencyCache) cleanupExpiredSlotKeysByPattern(ctx context.Context, pattern string) error {
	const scanCount = 200
	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, pattern, scanCount).Result()
		if err != nil {
			return fmt.Errorf("scan %s: %w", pattern, err)
		}
		if len(keys) > 0 {
			_, err := cleanupExpiredSlotKeysScript.Run(ctx, c.rdb, keys, c.slotTTLSeconds).Result()
			if err != nil {
				return fmt.Errorf("cleanup expired slots %s: %w", pattern, err)
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return nil
}

// cleanupSlotsByPattern 扫描匹配 pattern 的有序集合键，批量调用 Lua 脚本清理非当前进程成员。
func (c *concurrencyCache) cleanupSlotsByPattern(ctx context.Context, pattern, activePrefix string) error {
	const scanCount = 200
	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, pattern, scanCount).Result()
		if err != nil {
			return fmt.Errorf("scan %s: %w", pattern, err)
		}
		if len(keys) > 0 {
			_, err := startupCleanupScript.Run(ctx, c.rdb, keys, activePrefix, c.slotTTLSeconds).Result()
			if err != nil {
				return fmt.Errorf("cleanup slots %s: %w", pattern, err)
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return nil
}

// deleteKeysByPattern 扫描匹配 pattern 的键并删除。
func (c *concurrencyCache) deleteKeysByPattern(ctx context.Context, pattern string) error {
	const scanCount = 200
	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, pattern, scanCount).Result()
		if err != nil {
			return fmt.Errorf("scan %s: %w", pattern, err)
		}
		if len(keys) > 0 {
			if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
				return fmt.Errorf("del %s: %w", pattern, err)
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return nil
}
