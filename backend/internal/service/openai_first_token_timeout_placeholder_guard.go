package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/redis/go-redis/v9"
)

const (
	openAIFirstTokenTimeoutPlaceholderGuardStateTTL       = 30 * time.Minute
	openAIFirstTokenTimeoutPlaceholderGuardImmediateLimit = 20 * time.Millisecond
	openAIFirstTokenTimeoutPlaceholderGuardWriteLimit     = 200 * time.Millisecond
	openAIFirstTokenTimeoutPlaceholderGuardRetryInitial   = 50 * time.Millisecond
	openAIFirstTokenTimeoutPlaceholderGuardRetryMax       = time.Second
	openAIFirstTokenTimeoutPlaceholderGuardRetryBatchSize = 128
	openAIFirstTokenTimeoutPlaceholderGuardMaxPendingKeys = 65536
	openAIFirstTokenTimeoutPlaceholderGuardMaxLocalKeys   = 65536
	openAIFirstTokenTimeoutPlaceholderGuardOutboxPoll     = 5 * time.Second
	openAIFirstTokenTimeoutPlaceholderGuardKeyPrefix      = "openai:first_token_timeout_placeholder_guard:"
	openAIFirstTokenTimeoutPlaceholderGuardChannel        = "openai:first_token_timeout_placeholder_guard:updates"
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
	redis.call('PUBLISH', ARGV[4], ARGV[5])
	return 1
`)

type openAIFirstTokenTimeoutPlaceholderGuardSample struct {
	key         string
	realTokenMS int
	recordedAt  int64
}

type openAIFirstTokenTimeoutPlaceholderGuardLocalSample struct {
	realTokenMS int
	recordedAt  int64
	expiresAt   int64
}

type openAIFirstTokenTimeoutPlaceholderGuardUpdate struct {
	Key         string `json:"key"`
	RealTokenMS int    `json:"real_token_ms"`
	RecordedAt  int64  `json:"recorded_at"`
}

type openAIFirstTokenTimeoutPlaceholderGuard struct {
	redisClient           *redis.Client
	lastRedisErrorLogUnix atomic.Int64
	lastRecordedAt        atomic.Int64
	localMu               sync.RWMutex
	localSamples          map[string]openAIFirstTokenTimeoutPlaceholderGuardLocalSample
	retryMu               sync.Mutex
	retryPending          map[string]openAIFirstTokenTimeoutPlaceholderGuardSample
	retryRunning          bool
	retryOutbox           *openAIFirstTokenTimeoutPlaceholderGuardOutbox
	outboxWorkerOnce      sync.Once
	redisSyncWorkerOnce   sync.Once
	outboxWake            chan struct{}
	lifecycleMu           sync.Mutex
	workerStop            chan struct{}
	workerWG              sync.WaitGroup
	stopped               bool
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) setRedisClient(client *redis.Client) {
	if g == nil {
		return
	}
	g.redisClient = client
	if client != nil {
		g.redisSyncWorkerOnce.Do(func() {
			g.startWorker(g.runRedisSyncWorker)
		})
	}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) allow(accountID int64, model string, limitMS int) bool {
	if g == nil || accountID <= 0 || limitMS <= 0 {
		return true
	}

	key := openAIFirstTokenTimeoutPlaceholderGuardKey(accountID, model)
	now := time.Now().UnixNano()
	g.localMu.RLock()
	sample, ok := g.localSamples[key]
	g.localMu.RUnlock()
	if !ok {
		return true
	}
	if sample.expiresAt <= now {
		g.localMu.Lock()
		if current, exists := g.localSamples[key]; exists && current.expiresAt <= now {
			delete(g.localSamples, key)
		}
		g.localMu.Unlock()
		return true
	}
	return sample.realTokenMS <= limitMS
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) record(accountID int64, model string, realTokenMS int, limitMS int, recordedAt int64) {
	if g == nil || accountID <= 0 || realTokenMS <= 0 || limitMS <= 0 {
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
	g.updateLocalSample(sample, openAIFirstTokenTimeoutPlaceholderGuardStateTTL)
	if g.redisClient == nil {
		return
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
	payload, err := json.Marshal(openAIFirstTokenTimeoutPlaceholderGuardUpdate{
		Key:         sample.key,
		RealTokenMS: sample.realTokenMS,
		RecordedAt:  sample.recordedAt,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	applied, err := openAIFirstTokenTimeoutPlaceholderGuardWriteScript.Run(
		ctx,
		g.redisClient,
		[]string{sample.key},
		fmt.Sprintf("%020d", sample.recordedAt),
		sample.realTokenMS,
		openAIFirstTokenTimeoutPlaceholderGuardStateTTL.Milliseconds(),
		openAIFirstTokenTimeoutPlaceholderGuardChannel,
		string(payload),
	).Int()
	if err != nil {
		return err
	}
	if applied != 0 {
		g.updateLocalSample(sample, openAIFirstTokenTimeoutPlaceholderGuardStateTTL)
		return nil
	}
	// This runs only after a real first-token sample is committed. If another
	// replica already wrote a newer sample, reconcile the local snapshot here;
	// request-time allow() remains a memory-only operation.
	if err := g.refreshLocalSample(sample.key); err != nil {
		g.removeLocalSampleIfRecordedAt(sample.key, sample.recordedAt)
		return err
	}
	return nil
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) refreshLocalSample(key string) error {
	if g == nil || g.redisClient == nil || key == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), openAIFirstTokenTimeoutPlaceholderGuardWriteLimit)
	defer cancel()
	value, err := g.redisClient.Get(ctx, key).Result()
	if err != nil {
		return err
	}
	ttl, err := g.redisClient.PTTL(ctx, key).Result()
	if err != nil {
		return err
	}
	realTokenMS, recordedAt, err := decodeOpenAIFirstTokenTimeoutPlaceholderGuardStoredValue(value)
	if err != nil {
		return err
	}
	g.updateLocalSample(openAIFirstTokenTimeoutPlaceholderGuardSample{
		key:         key,
		realTokenMS: realTokenMS,
		recordedAt:  recordedAt,
	}, ttl)
	return nil
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) removeLocalSampleIfRecordedAt(key string, recordedAt int64) {
	if g == nil || key == "" || recordedAt <= 0 {
		return
	}
	g.localMu.Lock()
	if current, ok := g.localSamples[key]; ok && current.recordedAt == recordedAt {
		delete(g.localSamples, key)
	}
	g.localMu.Unlock()
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) updateLocalSample(sample openAIFirstTokenTimeoutPlaceholderGuardSample, ttl time.Duration) {
	if g == nil || sample.key == "" || sample.realTokenMS <= 0 || sample.recordedAt <= 0 || ttl <= 0 {
		return
	}
	now := time.Now()
	g.localMu.Lock()
	defer g.localMu.Unlock()
	if g.localSamples == nil {
		g.localSamples = make(map[string]openAIFirstTokenTimeoutPlaceholderGuardLocalSample)
	}
	if current, ok := g.localSamples[sample.key]; ok && current.recordedAt >= sample.recordedAt {
		return
	}
	if _, exists := g.localSamples[sample.key]; !exists && len(g.localSamples) >= openAIFirstTokenTimeoutPlaceholderGuardMaxLocalKeys {
		nowUnixNS := now.UnixNano()
		for key, current := range g.localSamples {
			if current.expiresAt <= nowUnixNS {
				delete(g.localSamples, key)
			}
		}
		if len(g.localSamples) >= openAIFirstTokenTimeoutPlaceholderGuardMaxLocalKeys {
			var oldestKey string
			var oldestExpiry int64
			for key, current := range g.localSamples {
				if oldestKey == "" || current.expiresAt < oldestExpiry {
					oldestKey = key
					oldestExpiry = current.expiresAt
				}
			}
			delete(g.localSamples, oldestKey)
		}
	}
	g.localSamples[sample.key] = openAIFirstTokenTimeoutPlaceholderGuardLocalSample{
		realTokenMS: sample.realTokenMS,
		recordedAt:  sample.recordedAt,
		expiresAt:   now.Add(ttl).UnixNano(),
	}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) startWorker(run func(<-chan struct{})) {
	if g == nil || run == nil {
		return
	}
	g.lifecycleMu.Lock()
	if g.stopped {
		g.lifecycleMu.Unlock()
		return
	}
	if g.workerStop == nil {
		g.workerStop = make(chan struct{})
	}
	stop := g.workerStop
	g.workerWG.Add(1)
	g.lifecycleMu.Unlock()
	go func() {
		defer g.workerWG.Done()
		run(stop)
	}()
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) stop() {
	if g == nil {
		return
	}
	g.lifecycleMu.Lock()
	if !g.stopped {
		g.stopped = true
		if g.workerStop != nil {
			close(g.workerStop)
		}
	}
	g.lifecycleMu.Unlock()
	g.workerWG.Wait()
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) runRedisSyncWorker(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		client := g.redisClient
		if client == nil {
			return
		}
		pubsub := client.Subscribe(context.Background(), openAIFirstTokenTimeoutPlaceholderGuardChannel)
		receiveCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := pubsub.Receive(receiveCtx)
		cancel()
		if err != nil {
			_ = pubsub.Close()
			g.logRedisError("subscribe", err)
			if !waitOpenAIFirstTokenTimeoutPlaceholderGuard(stop, time.Second) {
				return
			}
			continue
		}
		g.hydrateLocalSamples(stop)
		messages := pubsub.Channel()
		connected := true
		for connected {
			select {
			case <-stop:
				_ = pubsub.Close()
				return
			case message, ok := <-messages:
				if !ok {
					connected = false
					continue
				}
				g.applyRedisUpdate(message.Payload)
			}
		}
		_ = pubsub.Close()
	}
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) applyRedisUpdate(payload string) {
	var update openAIFirstTokenTimeoutPlaceholderGuardUpdate
	if err := json.Unmarshal([]byte(payload), &update); err != nil ||
		!strings.HasPrefix(update.Key, openAIFirstTokenTimeoutPlaceholderGuardKeyPrefix) {
		return
	}
	g.updateLocalSample(openAIFirstTokenTimeoutPlaceholderGuardSample{
		key:         update.Key,
		realTokenMS: update.RealTokenMS,
		recordedAt:  update.RecordedAt,
	}, openAIFirstTokenTimeoutPlaceholderGuardStateTTL)
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) hydrateLocalSamples(stop <-chan struct{}) {
	if g == nil || g.redisClient == nil {
		return
	}
	var cursor uint64
	loaded := 0
	for loaded < openAIFirstTokenTimeoutPlaceholderGuardMaxLocalKeys {
		select {
		case <-stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		keys, next, err := g.redisClient.Scan(ctx, cursor, openAIFirstTokenTimeoutPlaceholderGuardKeyPrefix+"*", 128).Result()
		cancel()
		if err != nil {
			g.logRedisError("hydrate scan", err)
			return
		}
		for _, key := range keys {
			if loaded >= openAIFirstTokenTimeoutPlaceholderGuardMaxLocalKeys {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			value, getErr := g.redisClient.Get(ctx, key).Result()
			ttl, ttlErr := g.redisClient.PTTL(ctx, key).Result()
			cancel()
			if getErr != nil || ttlErr != nil || ttl <= 0 {
				continue
			}
			realTokenMS, recordedAt, decodeErr := decodeOpenAIFirstTokenTimeoutPlaceholderGuardStoredValue(value)
			if decodeErr != nil {
				continue
			}
			g.updateLocalSample(openAIFirstTokenTimeoutPlaceholderGuardSample{
				key:         key,
				realTokenMS: realTokenMS,
				recordedAt:  recordedAt,
			}, ttl)
			loaded++
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}

func waitOpenAIFirstTokenTimeoutPlaceholderGuard(stop <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
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
	g.startWorker(g.runRetryWorker)
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) setRetryDB(db *sql.DB) {
	if g == nil || db == nil {
		return
	}
	g.retryOutbox = &openAIFirstTokenTimeoutPlaceholderGuardOutbox{db: db}
	g.outboxWorkerOnce.Do(func() {
		g.outboxWake = make(chan struct{}, 1)
		g.startWorker(g.runOutboxWorker)
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

func (g *openAIFirstTokenTimeoutPlaceholderGuard) runOutboxWorker(stop <-chan struct{}) {
	ticker := time.NewTicker(openAIFirstTokenTimeoutPlaceholderGuardOutboxPoll)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
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

func (g *openAIFirstTokenTimeoutPlaceholderGuard) runRetryWorker(stop <-chan struct{}) {
	backoff := openAIFirstTokenTimeoutPlaceholderGuardRetryInitial
	for {
		select {
		case <-stop:
			return
		default:
		}
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
		if !waitOpenAIFirstTokenTimeoutPlaceholderGuard(stop, backoff) {
			return
		}
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

func decodeOpenAIFirstTokenTimeoutPlaceholderGuardStoredValue(value string) (int, int64, error) {
	separator := strings.LastIndexByte(value, ':')
	if separator <= 0 || separator >= len(value)-1 {
		return 0, 0, fmt.Errorf("invalid guard value %q", value)
	}
	recordedAt, err := strconv.ParseInt(value[:separator], 10, 64)
	if err != nil || recordedAt <= 0 {
		return 0, 0, fmt.Errorf("invalid guard timestamp %q", value)
	}
	realTokenMS, err := decodeOpenAIFirstTokenTimeoutPlaceholderGuardValue(value)
	if err != nil {
		return 0, 0, err
	}
	return realTokenMS, recordedAt, nil
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
