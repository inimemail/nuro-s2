package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const stickySessionPrefix = "sticky_session:"
const openAIPromptCacheWarmPrefix = "openai_prompt_cache_warm:v1:"

var recordOpenAIPromptCacheWarmScript = redis.NewScript(`
local raw = redis.call('HGET', KEYS[1], ARGV[1])
if not raw and tonumber(ARGV[4]) == 0 then return 0 end
local entry = {account_id=tonumber(ARGV[2]), hit_rate_ewma=0.5, samples=0, input_tokens=0, cached_tokens=0, last_success_at=0, last_hit_at=0, avoid_until=0}
if raw then
  local ok, decoded = pcall(cjson.decode, raw)
  if ok and type(decoded) == 'table' then for k, v in pairs(decoded) do entry[k] = v end end
end
local input = tonumber(ARGV[3])
local cached = tonumber(ARGV[4])
local sample_rate = cached / input
if tonumber(entry.samples or 0) > 0 or cached > 0 then
  entry.hit_rate_ewma = tonumber(entry.hit_rate_ewma or 0.5) * 0.75 + sample_rate * 0.25
end
entry.account_id = tonumber(ARGV[2])
entry.samples = tonumber(entry.samples or 0) + 1
entry.input_tokens = tonumber(entry.input_tokens or 0) + input
entry.cached_tokens = tonumber(entry.cached_tokens or 0) + cached
local now = tonumber(ARGV[6])
entry.last_success_at = now
if cached > 0 then entry.last_hit_at = now; entry.avoid_until = 0 end
redis.call('HSET', KEYS[1], ARGV[1], cjson.encode(entry))
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[5]))
local all = redis.call('HGETALL', KEYS[1])
local limit = 3
if #all / 2 > limit then
  local ranked = {}
  for i = 1, #all, 2 do
    local ok, value = pcall(cjson.decode, all[i + 1])
    local score = -1
    if ok and type(value) == 'table' and tonumber(value.account_id or 0) > 0 then
      local confidence = math.min(tonumber(value.samples or 0) / 3, 1)
      local age_hours = (now - tonumber(value.last_success_at or 0)) / 3600
      local decay = math.exp(-age_hours * math.log(2) / 12)
      score = tonumber(value.hit_rate_ewma or 0) * confidence * decay
    end
    table.insert(ranked, {field=all[i], score=score})
  end
  table.sort(ranked, function(a, b) return a.score > b.score end)
  for i = limit + 1, #ranked do redis.call('HDEL', KEYS[1], ranked[i].field) end
end
return 1
`)

var avoidOpenAIPromptCacheWarmScript = redis.NewScript(`
local raw = redis.call('HGET', KEYS[1], ARGV[1])
if not raw then return 0 end
local ok, entry = pcall(cjson.decode, raw)
if not ok or type(entry) ~= 'table' or tonumber(entry.account_id or 0) <= 0 then return 0 end
local until_unix = tonumber(ARGV[2])
if until_unix <= tonumber(entry.avoid_until or 0) then return 0 end
entry.avoid_until = until_unix
redis.call('HSET', KEYS[1], ARGV[1], cjson.encode(entry))
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
return 1
`)

type gatewayCache struct {
	rdb *redis.Client
}

func NewGatewayCache(rdb *redis.Client) service.GatewayCache {
	return &gatewayCache{rdb: rdb}
}

// buildSessionKey 构建 session key，包含 groupID 实现分组隔离
// 格式: sticky_session:{groupID}:{sessionHash}
func buildSessionKey(groupID int64, sessionHash string) string {
	return fmt.Sprintf("%s%d:%s", stickySessionPrefix, groupID, sessionHash)
}

func (c *gatewayCache) GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error) {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Get(ctx, key).Int64()
}

func (c *gatewayCache) SetSessionAccountID(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Set(ctx, key, accountID, ttl).Err()
}

// SetSessionAccountIDIfAbsent claims a sticky binding without allowing
// concurrent first requests on different replicas to overwrite each other.
func (c *gatewayCache) SetSessionAccountIDIfAbsent(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) (bool, error) {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.SetNX(ctx, key, accountID, ttl).Result()
}

func (c *gatewayCache) RefreshSessionTTL(ctx context.Context, groupID int64, sessionHash string, ttl time.Duration) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Expire(ctx, key, ttl).Err()
}

// DeleteSessionAccountID 删除粘性会话与账号的绑定关系。
// 当检测到绑定的账号不可用（如状态错误、禁用、不可调度等）时调用，
// 以便下次请求能够重新选择可用账号。
//
// DeleteSessionAccountID removes the sticky session binding for the given session.
// Called when the bound account becomes unavailable (e.g., error status, disabled,
// or unschedulable), allowing subsequent requests to select a new available account.
func (c *gatewayCache) DeleteSessionAccountID(ctx context.Context, groupID int64, sessionHash string) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Del(ctx, key).Err()
}

func buildOpenAIPromptCacheWarmKey(groupID int64, affinityHash string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(affinityHash)))
	return fmt.Sprintf("%s%d:%s", openAIPromptCacheWarmPrefix, groupID, hex.EncodeToString(sum[:16]))
}

func (c *gatewayCache) GetOpenAIPromptCacheWarmAccounts(ctx context.Context, groupID int64, affinityHash string) ([]service.OpenAIPromptCacheWarmAccount, error) {
	if strings.TrimSpace(affinityHash) == "" {
		return nil, nil
	}
	values, err := c.rdb.HGetAll(ctx, buildOpenAIPromptCacheWarmKey(groupID, affinityHash)).Result()
	if err != nil {
		return nil, err
	}
	entries := make([]service.OpenAIPromptCacheWarmAccount, 0, len(values))
	for _, raw := range values {
		var entry service.OpenAIPromptCacheWarmAccount
		if json.Unmarshal([]byte(raw), &entry) == nil && entry.AccountID > 0 {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (c *gatewayCache) RecordOpenAIPromptCacheWarmResult(ctx context.Context, groupID int64, affinityHash string, accountID int64, inputTokens int, cachedTokens int, ttl time.Duration) error {
	if strings.TrimSpace(affinityHash) == "" || accountID <= 0 || inputTokens <= 0 || ttl <= 0 {
		return nil
	}
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > inputTokens {
		cachedTokens = inputTokens
	}
	key := buildOpenAIPromptCacheWarmKey(groupID, affinityHash)
	field := strconv.FormatInt(accountID, 10)
	ttlSeconds := int64(ttl / time.Second)
	if ttlSeconds <= 0 {
		ttlSeconds = 1
	}
	_, err := recordOpenAIPromptCacheWarmScript.Run(ctx, c.rdb, []string{key},
		field, accountID, inputTokens, cachedTokens, ttlSeconds, time.Now().Unix()).Result()
	return err
}

func (c *gatewayCache) AvoidOpenAIPromptCacheWarmAccount(ctx context.Context, groupID int64, affinityHash string, accountID int64, until time.Time, ttl time.Duration) error {
	if strings.TrimSpace(affinityHash) == "" || accountID <= 0 || until.IsZero() || ttl <= 0 {
		return nil
	}
	key := buildOpenAIPromptCacheWarmKey(groupID, affinityHash)
	field := strconv.FormatInt(accountID, 10)
	ttlSeconds := int64(ttl / time.Second)
	if ttlSeconds <= 0 {
		ttlSeconds = 1
	}
	_, err := avoidOpenAIPromptCacheWarmScript.Run(ctx, c.rdb, []string{key}, field, until.Unix(), ttlSeconds).Result()
	return err
}
