package service

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

const (
	openAIFirstTokenTimeoutPlaceholderGuardStateTTL       = 30 * time.Minute
	openAIFirstTokenTimeoutPlaceholderGuardReadLimit      = 20 * time.Millisecond
	openAIFirstTokenTimeoutPlaceholderGuardImmediateLimit = 20 * time.Millisecond
	openAIFirstTokenTimeoutPlaceholderGuardWriteLimit     = 200 * time.Millisecond
	openAIFirstTokenTimeoutPlaceholderGuardRetryInitial   = 50 * time.Millisecond
	openAIFirstTokenTimeoutPlaceholderGuardRetryMax       = time.Second
	openAIFirstTokenTimeoutPlaceholderGuardRetryBatchSize = 128
	openAIFirstTokenTimeoutPlaceholderGuardMaxPendingKeys = 65536
	openAIFirstTokenTimeoutPlaceholderGuardOutboxPoll     = 5 * time.Second
	openAIFirstTokenTimeoutPlaceholderGuardKeyPrefix      = "openai:first_token_timeout_placeholder_guard:"
)

var openAIFirstTokenTimeoutPlaceholderGuardWriteScript = redis.NewScript(`
	local current = redis.call('GET', KEYS[1])
	local incoming_time = ARGV[1]
	if current then
		local separator = string.find(current, ':', 1, true)
		if separator then
			local current_time = string.sub(current, 1, separator - 1)
			if current_time and current_time > incoming_time then
				return 0
			end
		end
	end
	redis.call('SET', KEYS[1], ARGV[1] .. ':' .. ARGV[2], 'PX', ARGV[3])
	return 1
`)

type openAIFirstTokenTimeoutPlaceholderGuardSample struct {
	key         string
	realTokenMS int
	recordedAt  int64
}

type openAIFirstTokenTimeoutPlaceholderGuard struct {
	redisClient           *redis.Client
	lastRedisErrorLogUnix atomic.Int64
	lastRecordedAt        atomic.Int64
	readGroup             singleflight.Group
	retryMu               sync.Mutex
	retryPending          map[string]openAIFirstTokenTimeoutPlaceholderGuardSample
	retryRunning          bool
	retryOutbox           *openAIFirstTokenTimeoutPlaceholderGuardOutbox
	outboxWorkerOnce      sync.Once
	outboxWake            chan struct{}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) setRedisClient(client *redis.Client) {
	if g == nil {
		return
	}
	g.redisClient = client
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) allow(accountID int64, model string, limitMS int) bool {
	if g == nil || accountID <= 0 || limitMS <= 0 || g.redisClient == nil {
		return true
	}

	key := openAIFirstTokenTimeoutPlaceholderGuardKey(accountID, model)
	value, err, _ := g.readGroup.Do(key, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), openAIFirstTokenTimeoutPlaceholderGuardReadLimit)
		defer cancel()
		realTokenValue, err := g.redisClient.Get(ctx, key).Result()
		if err == redis.Nil {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		realTokenMS, err := decodeOpenAIFirstTokenTimeoutPlaceholderGuardValue(realTokenValue)
		if err != nil {
			return 0, err
		}
		return realTokenMS, nil
	})
	if err != nil {
		g.logRedisError("read", err)
		return true
	}
	realTokenMS, ok := value.(int)
	if !ok || realTokenMS <= 0 {
		return true
	}
	return realTokenMS <= limitMS
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) record(accountID int64, model string, realTokenMS int, limitMS int, recordedAt int64) {
	if g == nil || accountID <= 0 || realTokenMS <= 0 || limitMS <= 0 || g.redisClient == nil {
		return
	}

	if recordedAt <= 0 {
		recordedAt = g.nextRecordedAt()
	}
	sample := openAIFirstTokenTimeoutPlaceholderGuardSample{
		key:         openAIFirstTokenTimeoutPlaceholderGuardKey(accountID, model),
		realTokenMS: realTokenMS,
		recordedAt:  recordedAt,
	}
	if err := g.writeSampleWithLimit(sample, openAIFirstTokenTimeoutPlaceholderGuardImmediateLimit); err != nil {
		g.logRedisError("write", err)
		g.enqueueRetry(sample)
	}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) nextRecordedAt() int64 {
	now := time.Now().UnixNano()
	for {
		last := g.lastRecordedAt.Load()
		if now <= last {
			now = last + 1
		}
		if g.lastRecordedAt.CompareAndSwap(last, now) {
			return now
		}
	}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) writeSample(sample openAIFirstTokenTimeoutPlaceholderGuardSample) error {
	return g.writeSampleWithLimit(sample, openAIFirstTokenTimeoutPlaceholderGuardWriteLimit)
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) writeSampleWithLimit(sample openAIFirstTokenTimeoutPlaceholderGuardSample, timeout time.Duration) error {
	if g == nil || g.redisClient == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return openAIFirstTokenTimeoutPlaceholderGuardWriteScript.Run(
		ctx,
		g.redisClient,
		[]string{sample.key},
		fmt.Sprintf("%020d", sample.recordedAt),
		sample.realTokenMS,
		openAIFirstTokenTimeoutPlaceholderGuardStateTTL.Milliseconds(),
	).Err()
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) enqueueRetry(sample openAIFirstTokenTimeoutPlaceholderGuardSample) {
	if g == nil {
		return
	}
	if g.retryOutbox != nil {
		ctx, cancel := context.WithTimeout(context.Background(), openAIFirstTokenTimeoutPlaceholderGuardOutboxDBLimit)
		err := g.retryOutbox.upsert(ctx, sample)
		cancel()
		if err == nil {
			g.signalOutboxWorker()
			return
		}
		g.logRedisError("persist retry", err)
	}
	g.enqueueMemoryRetry(sample)
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) enqueueMemoryRetry(sample openAIFirstTokenTimeoutPlaceholderGuardSample) {
	if g == nil {
		return
	}
	g.retryMu.Lock()
	if g.retryPending == nil {
		g.retryPending = make(map[string]openAIFirstTokenTimeoutPlaceholderGuardSample)
	}
	if current, ok := g.retryPending[sample.key]; ok && current.recordedAt >= sample.recordedAt {
		g.retryMu.Unlock()
		return
	}
	if _, ok := g.retryPending[sample.key]; !ok && len(g.retryPending) >= openAIFirstTokenTimeoutPlaceholderGuardMaxPendingKeys {
		g.retryMu.Unlock()
		g.logRedisError("retry queue full", fmt.Errorf("pending key limit %d reached", openAIFirstTokenTimeoutPlaceholderGuardMaxPendingKeys))
		return
	}
	g.retryPending[sample.key] = sample
	if g.retryRunning {
		g.retryMu.Unlock()
		return
	}
	g.retryRunning = true
	g.retryMu.Unlock()
	go g.runRetryWorker()
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) setRetryDB(db *sql.DB) {
	if g == nil || db == nil {
		return
	}
	g.retryOutbox = &openAIFirstTokenTimeoutPlaceholderGuardOutbox{db: db}
	g.outboxWorkerOnce.Do(func() {
		g.outboxWake = make(chan struct{}, 1)
		go g.runOutboxWorker()
	})
	g.signalOutboxWorker()
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) signalOutboxWorker() {
	if g == nil || g.outboxWake == nil {
		return
	}
	select {
	case g.outboxWake <- struct{}{}:
	default:
	}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) runOutboxWorker() {
	ticker := time.NewTicker(openAIFirstTokenTimeoutPlaceholderGuardOutboxPoll)
	defer ticker.Stop()
	for {
		select {
		case <-g.outboxWake:
		case <-ticker.C:
		}
		g.drainOutbox()
	}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) drainOutbox() {
	if g == nil || g.retryOutbox == nil {
		return
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), openAIFirstTokenTimeoutPlaceholderGuardOutboxDBLimit)
		samples, err := g.retryOutbox.list(ctx, openAIFirstTokenTimeoutPlaceholderGuardRetryBatchSize)
		cancel()
		if err != nil {
			g.logRedisError("load persistent retry", err)
			return
		}
		if len(samples) == 0 {
			return
		}
		staleSample := false
		for _, sample := range samples {
			if err := g.writeSample(sample); err != nil {
				g.logRedisError("persistent retry", err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), openAIFirstTokenTimeoutPlaceholderGuardOutboxDBLimit)
			deleted, err := g.retryOutbox.delete(ctx, sample)
			cancel()
			if err != nil {
				g.logRedisError("delete persistent retry", err)
				return
			}
			if !deleted {
				staleSample = true
			}
		}
		if staleSample {
			continue
		}
		if len(samples) < openAIFirstTokenTimeoutPlaceholderGuardRetryBatchSize {
			return
		}
	}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) runRetryWorker() {
	backoff := openAIFirstTokenTimeoutPlaceholderGuardRetryInitial
	for {
		samples := g.retrySnapshot()
		if len(samples) == 0 {
			g.retryMu.Lock()
			if len(g.retryPending) == 0 {
				g.retryRunning = false
				g.retryMu.Unlock()
				return
			}
			g.retryMu.Unlock()
			continue
		}

		wroteAny := false
		for _, sample := range samples {
			if err := g.writeSample(sample); err != nil {
				g.logRedisError("retry", err)
				continue
			}
			wroteAny = true
			g.clearRetriedSample(sample)
		}
		if wroteAny {
			backoff = openAIFirstTokenTimeoutPlaceholderGuardRetryInitial
			continue
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, openAIFirstTokenTimeoutPlaceholderGuardRetryMax)
	}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) retrySnapshot() []openAIFirstTokenTimeoutPlaceholderGuardSample {
	g.retryMu.Lock()
	defer g.retryMu.Unlock()
	capacity := min(len(g.retryPending), openAIFirstTokenTimeoutPlaceholderGuardRetryBatchSize)
	samples := make([]openAIFirstTokenTimeoutPlaceholderGuardSample, 0, capacity)
	for _, sample := range g.retryPending {
		samples = append(samples, sample)
		if len(samples) >= capacity {
			break
		}
	}
	return samples
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) clearRetriedSample(sample openAIFirstTokenTimeoutPlaceholderGuardSample) {
	g.retryMu.Lock()
	defer g.retryMu.Unlock()
	if current, ok := g.retryPending[sample.key]; ok && current.recordedAt == sample.recordedAt {
		delete(g.retryPending, sample.key)
	}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) logRedisError(operation string, err error) {
	if g == nil || err == nil {
		return
	}
	now := time.Now().Unix()
	for {
		last := g.lastRedisErrorLogUnix.Load()
		if now-last < 60 {
			return
		}
		if g.lastRedisErrorLogUnix.CompareAndSwap(last, now) {
			logger.LegacyPrintf("service.openai_gateway", "First token placeholder guard Redis %s failed; allowing timeout placeholder: %v", operation, err)
			return
		}
	}
}

func openAIFirstTokenTimeoutPlaceholderGuardKey(accountID int64, model string) string {
	return fmt.Sprintf("%s%d:%s", openAIFirstTokenTimeoutPlaceholderGuardKeyPrefix, accountID, normalizeOpenAIFirstTokenTimeoutPlaceholderGuardModel(model))
}

func decodeOpenAIFirstTokenTimeoutPlaceholderGuardValue(value string) (int, error) {
	realTokenValue := value
	if separator := strings.LastIndexByte(value, ':'); separator >= 0 {
		realTokenValue = value[separator+1:]
	}
	realTokenMS, err := strconv.Atoi(realTokenValue)
	if err != nil || realTokenMS <= 0 {
		return 0, fmt.Errorf("invalid real token value %q", value)
	}
	return realTokenMS, nil
}

func normalizeOpenAIFirstTokenTimeoutPlaceholderGuardModel(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return "*"
	}
	return normalized
}

func (s *OpenAIGatewayService) SetOpenAIFirstTokenTimeoutPlaceholderGuardRedisClient(client *redis.Client) {
	if s == nil {
		return
	}
	s.openaiFirstTokenTimeoutPlaceholderGuard.setRedisClient(client)
}

func (s *OpenAIGatewayService) SetOpenAIFirstTokenTimeoutPlaceholderGuardRetryDB(db *sql.DB) {
	if s == nil {
		return
	}
	s.openaiFirstTokenTimeoutPlaceholderGuard.setRetryDB(db)
}
