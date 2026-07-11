package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const stickySessionPrefix = "sticky_session:"
const openAIPromptCacheWarmPrefix = "openai_prompt_cache_warm:v1:"

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
	entry := service.OpenAIPromptCacheWarmAccount{AccountID: accountID, HitRateEWMA: 0.5}
	if raw, err := c.rdb.HGet(ctx, key, field).Result(); err == nil {
		_ = json.Unmarshal([]byte(raw), &entry)
	} else if err == redis.Nil && cachedTokens == 0 {
		return nil
	} else if err != redis.Nil {
		return err
	}
	entry.AccountID = accountID
	sampleRate := float64(cachedTokens) / float64(inputTokens)
	if entry.Samples > 0 || cachedTokens > 0 {
		entry.HitRateEWMA = entry.HitRateEWMA*0.75 + sampleRate*0.25
	}
	entry.Samples++
	entry.InputTokens += int64(inputTokens)
	entry.CachedTokens += int64(cachedTokens)
	now := time.Now()
	entry.LastSuccessAt = now.Unix()
	if cachedTokens > 0 {
		entry.LastHitAt = now.Unix()
		entry.AvoidUntil = 0
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	pipe := c.rdb.TxPipeline()
	pipe.HSet(ctx, key, field, encoded)
	pipe.Expire(ctx, key, ttl)
	if _, err = pipe.Exec(ctx); err != nil {
		return err
	}
	return c.pruneOpenAIPromptCacheWarmAccounts(ctx, key, 3)
}

func (c *gatewayCache) AvoidOpenAIPromptCacheWarmAccount(ctx context.Context, groupID int64, affinityHash string, accountID int64, until time.Time, ttl time.Duration) error {
	if strings.TrimSpace(affinityHash) == "" || accountID <= 0 || until.IsZero() || ttl <= 0 {
		return nil
	}
	key := buildOpenAIPromptCacheWarmKey(groupID, affinityHash)
	field := strconv.FormatInt(accountID, 10)
	raw, err := c.rdb.HGet(ctx, key, field).Result()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		return err
	}
	var entry service.OpenAIPromptCacheWarmAccount
	if json.Unmarshal([]byte(raw), &entry) != nil || entry.AccountID <= 0 {
		return nil
	}
	if until.Unix() > entry.AvoidUntil {
		entry.AvoidUntil = until.Unix()
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	pipe := c.rdb.TxPipeline()
	pipe.HSet(ctx, key, field, encoded)
	pipe.Expire(ctx, key, ttl)
	_, err = pipe.Exec(ctx)
	return err
}

func (c *gatewayCache) pruneOpenAIPromptCacheWarmAccounts(ctx context.Context, key string, limit int) error {
	if limit <= 0 {
		return nil
	}
	values, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil || len(values) <= limit {
		return err
	}
	type rankedEntry struct {
		field string
		score float64
	}
	now := time.Now()
	ranked := make([]rankedEntry, 0, len(values))
	for field, raw := range values {
		var entry service.OpenAIPromptCacheWarmAccount
		if json.Unmarshal([]byte(raw), &entry) != nil || entry.AccountID <= 0 {
			ranked = append(ranked, rankedEntry{field: field, score: -1})
			continue
		}
		confidence := math.Min(float64(entry.Samples)/3, 1)
		age := now.Sub(time.Unix(entry.LastSuccessAt, 0))
		decay := math.Exp(-age.Hours() * math.Ln2 / 12)
		ranked = append(ranked, rankedEntry{field: field, score: entry.HitRateEWMA * confidence * decay})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	fields := make([]string, 0, len(ranked)-limit)
	for _, item := range ranked[limit:] {
		fields = append(fields, item.field)
	}
	if len(fields) == 0 {
		return nil
	}
	return c.rdb.HDel(ctx, key, fields...).Err()
}
