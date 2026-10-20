package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	openAIAccountHealthSharedChannel   = "sub2api:openai:health:v1"
	openAIAccountHealthSharedKeyPrefix = "sub2api:openai:health:v1:"
	openAIAccountHealthResetKeyPrefix  = "sub2api:openai:health-reset:v1:"
	openAIAccountHealthSharedTTL       = time.Duration(MaxAdaptiveHealthSampleFreshnessMinutes) * time.Minute
	openAIAccountHealthSharedQueueSize = 4096
	openAIAccountHealthLoadQueueSize   = 1024
	openAIAccountHealthLoadWorkers     = 2
	openAIAccountHealthRefreshInterval = 15 * time.Second
	openAIAccountHealthRefreshBatch    = 256
	openAIAccountHealthMissingKeyLimit = 4096
	openAIAccountHealthIOTimeout       = 2 * time.Second
)

var openAIAccountHealthUpdateScript = redis.NewScript(`
if KEYS[3] ~= '' then
  local previous = redis.call('GET', KEYS[3])
  if previous then return cjson.decode(previous) end
end
local error_sample = tonumber(ARGV[1])
local ttft_sample = ARGV[2]
local report_ns = ARGV[3]
local ttl_seconds = tonumber(ARGV[4])
local alpha = 0.2
local reset_ns = redis.call('GET', KEYS[2]) or '0'
local function decimal_gt(a, b)
  a = tostring(a or '0'):gsub('^0+', '')
  b = tostring(b or '0'):gsub('^0+', '')
  if a == '' then a = '0' end
  if b == '' then b = '0' end
  if string.len(a) ~= string.len(b) then
    return string.len(a) > string.len(b)
  end
  return a > b
end

local old_error = redis.call('HGET', KEYS[1], 'error_rate')
local error_rate = tonumber(old_error or '0')
local sample_count = tonumber(redis.call('HGET', KEYS[1], 'sample_count') or '0')
local error_updated_ns = redis.call('HGET', KEYS[1], 'error_updated_ns') or redis.call('HGET', KEYS[1], 'updated_ns') or '0'
if not decimal_gt(error_updated_ns, reset_ns) then
  -- Reset scans are bounded and may stop partway through a large account.
  -- Enforce the fence here too, so the first post-reset sample never blends
  -- with error history that the scan did not reach.
  old_error = nil
  error_rate = 0
  sample_count = 0
  error_updated_ns = '0'
end
if decimal_gt(report_ns, reset_ns) then
  error_rate = error_sample
  if old_error and sample_count > 0 then
    error_rate = alpha * error_sample + (1 - alpha) * tonumber(old_error)
  end
  sample_count = sample_count + 1
  if decimal_gt(report_ns, error_updated_ns) then
    error_updated_ns = report_ns
  end
end

local ttft_sample_count = tonumber(redis.call('HGET', KEYS[1], 'ttft_sample_count') or '0')
local ttft = redis.call('HGET', KEYS[1], 'ttft')
local ttft_updated_ns = redis.call('HGET', KEYS[1], 'ttft_updated_ns') or '0'
if ttft_sample ~= '' then
  local sample = tonumber(ttft_sample)
  if ttft then
    ttft = alpha * sample + (1 - alpha) * tonumber(ttft)
  else
    ttft = sample
  end
  ttft_sample_count = redis.call('HINCRBY', KEYS[1], 'ttft_sample_count', 1)
  if decimal_gt(report_ns, ttft_updated_ns) then
    ttft_updated_ns = report_ns
  end
end

local version = redis.call('HINCRBY', KEYS[1], 'version', 1)
local updated_ns = redis.call('HGET', KEYS[1], 'updated_ns') or '0'
if decimal_gt(report_ns, updated_ns) then
  updated_ns = report_ns
end
redis.call('HSET', KEYS[1],
  'error_rate', tostring(error_rate),
  'sample_count', tostring(sample_count),
  'updated_ns', updated_ns,
  'error_updated_ns', error_updated_ns,
  'ttft_sample_count', tostring(ttft_sample_count))
if ttft then
  redis.call('HSET', KEYS[1], 'ttft', tostring(ttft), 'ttft_updated_ns', ttft_updated_ns)
end
redis.call('EXPIRE', KEYS[1], ttl_seconds)

local result = {tostring(error_rate), ttft or '', sample_count, ttft_sample_count, updated_ns, version, error_updated_ns, ttft_updated_ns}
if KEYS[3] ~= '' then
  redis.call('SET', KEYS[3], cjson.encode(result), 'EX', 30)
end
return result
`)

var openAIAccountHealthResetFenceScript = redis.NewScript(`
local requested_ns = ARGV[1]
local current_ns = redis.call('GET', KEYS[1]) or '0'
local function decimal_gt(a, b)
  a = tostring(a or '0'):gsub('^0+', '')
  b = tostring(b or '0'):gsub('^0+', '')
  if a == '' then a = '0' end
  if b == '' then b = '0' end
  if string.len(a) ~= string.len(b) then
    return string.len(a) > string.len(b)
  end
  return a > b
end
if decimal_gt(requested_ns, current_ns) then
  current_ns = requested_ns
  redis.call('SET', KEYS[1], current_ns)
end
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]))
return current_ns
`)

var openAIAccountHealthResetErrorScript = redis.NewScript(`
local reset_ns = ARGV[1]
local current_ns = redis.call('HGET', KEYS[1], 'error_updated_ns') or redis.call('HGET', KEYS[1], 'updated_ns') or '0'
local function decimal_gt(a, b)
  a = tostring(a or '0'):gsub('^0+', '')
  b = tostring(b or '0'):gsub('^0+', '')
  if a == '' then a = '0' end
  if b == '' then b = '0' end
  if string.len(a) ~= string.len(b) then
    return string.len(a) > string.len(b)
  end
  return a > b
end
if decimal_gt(current_ns, reset_ns) then
  return 0
end
redis.call('HSET', KEYS[1], 'error_rate', '0', 'sample_count', '0', 'error_updated_ns', '0')
redis.call('HINCRBY', KEYS[1], 'version', 1)
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]))
return 1
`)

type openAIAccountHealthSharedReport struct {
	reportID    string
	key         openAIAccountRuntimeStatsKey
	success     bool
	ttftMS      *int
	updatedNano int64
}

type openAIAccountHealthSharedEvent struct {
	ResetError       bool    `json:"reset_error,omitempty"`
	AccountID        int64   `json:"account_id"`
	Kind             string  `json:"kind"`
	Model            string  `json:"model"`
	Transport        string  `json:"transport"`
	ErrorRate        float64 `json:"error_rate"`
	TTFT             float64 `json:"ttft"`
	HasTTFT          bool    `json:"has_ttft"`
	SampleCount      int64   `json:"sample_count"`
	TTFTSampleCount  int64   `json:"ttft_sample_count"`
	UpdatedNano      int64   `json:"updated_nano"`
	ErrorUpdatedNano int64   `json:"error_updated_nano"`
	TTFTUpdatedNano  int64   `json:"ttft_updated_nano"`
	Version          int64   `json:"version"`
}

type openAIAccountHealthSharedState struct {
	client        *redis.Client
	stats         *openAIAccountRuntimeStats
	reports       chan openAIAccountHealthSharedReport
	loads         chan openAIAccountRuntimeStatsKey
	ctx           context.Context
	cancel        context.CancelFunc
	lifecycleMu   sync.Mutex
	closed        bool
	startOnce     sync.Once
	closeOnce     sync.Once
	workers       sync.WaitGroup
	loadOnce      sync.Map
	reportDrops   atomic.Int64
	persistErrors atomic.Int64
	publishErrors atomic.Int64
	loadErrors    atomic.Int64
	missingMu     sync.Mutex
	missingUntil  map[openAIAccountRuntimeStatsKey]int64
}

func newOpenAIAccountHealthSharedState(client *redis.Client, stats *openAIAccountRuntimeStats, queueSize int) *openAIAccountHealthSharedState {
	if queueSize <= 0 {
		queueSize = openAIAccountHealthSharedQueueSize
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &openAIAccountHealthSharedState{
		client:       client,
		stats:        stats,
		reports:      make(chan openAIAccountHealthSharedReport, queueSize),
		loads:        make(chan openAIAccountRuntimeStatsKey, openAIAccountHealthLoadQueueSize),
		ctx:          ctx,
		cancel:       cancel,
		missingUntil: make(map[openAIAccountRuntimeStatsKey]int64),
	}
}

func (s *openAIAccountRuntimeStats) setSharedRedisClient(client *redis.Client) {
	if s == nil || client == nil {
		return
	}
	s.sharedMu.Lock()
	shared := s.shared
	created := false
	if shared != nil && shared.client == client {
		s.sharedMu.Unlock()
		return
	}
	if shared == nil {
		shared = newOpenAIAccountHealthSharedState(client, s, openAIAccountHealthSharedQueueSize)
		s.shared = shared
		created = true
	}
	s.sharedMu.Unlock()
	if !created {
		return
	}
	shared.start()
	s.accounts.Range(func(rawKey, _ any) bool {
		if key, ok := rawKey.(openAIAccountRuntimeStatsKey); ok {
			shared.load(key)
		}
		return true
	})
}

func (s *openAIAccountRuntimeStats) sharedHealthState() *openAIAccountHealthSharedState {
	if s == nil {
		return nil
	}
	s.sharedMu.RLock()
	shared := s.shared
	s.sharedMu.RUnlock()
	return shared
}

func (s *openAIAccountRuntimeStats) closeSharedHealth() {
	if s == nil {
		return
	}
	s.sharedMu.RLock()
	shared := s.shared
	s.sharedMu.RUnlock()
	if shared != nil {
		shared.close()
	}
}

func (s *openAIAccountRuntimeStats) enqueueSharedHealthReport(key openAIAccountRuntimeStatsKey, success bool, firstTokenMS *int, updatedNano int64) {
	shared := s.sharedHealthState()
	if shared == nil {
		return
	}
	key = normalizeOpenAIAccountRuntimeStatsKey(key)
	report := openAIAccountHealthSharedReport{reportID: uuid.NewString(), key: key, success: success, updatedNano: updatedNano}
	if firstTokenMS != nil && *firstTokenMS > 0 {
		value := *firstTokenMS
		report.ttftMS = &value
	}
	select {
	case shared.reports <- report:
	default:
		if dropped := shared.reportDrops.Add(1); dropped == 1 || dropped%1024 == 0 {
			slog.Warn("openai shared health report queue full", "dropped_total", dropped)
		}
	}
}

func (s *openAIAccountRuntimeStats) loadSharedHealthOnce(key openAIAccountRuntimeStatsKey, stat *openAIAccountRuntimeStat) {
	shared := s.sharedHealthState()
	if shared == nil || stat == nil {
		return
	}
	shared.load(key)
}

func (s *openAIAccountHealthSharedState) start() {
	if s == nil || s.client == nil || s.stats == nil || s.ctx == nil {
		return
	}
	s.startOnce.Do(func() {
		s.lifecycleMu.Lock()
		if s.closed {
			s.lifecycleMu.Unlock()
			return
		}
		s.workers.Add(2 + openAIAccountHealthLoadWorkers)
		s.lifecycleMu.Unlock()
		go func() {
			defer s.workers.Done()
			s.runPublisher()
		}()
		go func() {
			defer s.workers.Done()
			s.runSubscriber()
		}()
		for range openAIAccountHealthLoadWorkers {
			go func() {
				defer s.workers.Done()
				s.runLoader()
			}()
		}
	})
}

func (s *openAIAccountHealthSharedState) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.closed = true
		if s.cancel != nil {
			s.cancel()
		}
		s.lifecycleMu.Unlock()
		s.workers.Wait()
	})
}

func (s *openAIAccountHealthSharedState) runPublisher() {
	for {
		var report openAIAccountHealthSharedReport
		select {
		case <-s.ctx.Done():
			return
		case report = <-s.reports:
		}
		// Retry with a short-lived idempotency receipt: an acknowledged-late Lua
		// write must never count the same sample twice.
		var event openAIAccountHealthSharedEvent
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			event, err = s.persist(report)
			if err == nil || s.ctx.Err() != nil {
				break
			}
			s.persistErrors.Add(1)
			if attempt < 2 && !waitOpenAISharedHealthRetry(s.ctx, time.Duration(attempt+1)*100*time.Millisecond) {
				return
			}
		}
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			if failures := s.persistErrors.Load(); failures <= 3 || failures%1024 == 0 {
				slog.Warn("openai shared health update failed after bounded retries", "error", err, "persist_errors_total", failures)
			}
			continue
		}
		s.stats.applySharedHealthEvent(event)
		payload, err := json.Marshal(event)
		if err != nil {
			continue
		}
		for attempt := 0; attempt < 2; attempt++ {
			ctx, cancel := context.WithTimeout(s.ctx, openAIAccountHealthIOTimeout)
			err = s.client.WithTimeout(openAIAccountHealthIOTimeout).Publish(ctx, openAIAccountHealthSharedChannel, payload).Err()
			cancel()
			if err == nil || s.ctx.Err() != nil {
				break
			}
			s.publishErrors.Add(1)
			if attempt == 0 && !waitOpenAISharedHealthRetry(s.ctx, 100*time.Millisecond) {
				return
			}
		}
		if err != nil {
			if failures := s.publishErrors.Load(); failures <= 2 || failures%1024 == 0 {
				slog.Warn("openai shared health publish failed after bounded retries", "error", err, "publish_errors_total", failures)
			}
		}
	}
}

func (s *openAIAccountHealthSharedState) runSubscriber() {
	// PubSub needs its own initialized pool: go-redis WithTimeout clones are
	// suitable for commands, but do not initialize a subscription pool. Keep
	// these socket/deadline options isolated from scheduling and billing I/O.
	options := *s.client.Options()
	options.ReadTimeout = openAIAccountHealthIOTimeout
	options.WriteTimeout = openAIAccountHealthIOTimeout
	options.ContextTimeoutEnabled = true
	options.PoolSize = 1
	options.MinIdleConns = 0 // The dedicated PubSub socket needs no idle command pool.
	if options.DialTimeout <= 0 || options.DialTimeout > openAIAccountHealthIOTimeout {
		options.DialTimeout = openAIAccountHealthIOTimeout
	}
	subscriberClient := redis.NewClient(&options)
	defer func() { _ = subscriberClient.Close() }()
	ticker := time.NewTicker(openAIAccountHealthRefreshInterval)
	defer ticker.Stop()
subscribeLoop:
	for {
		if s.ctx.Err() != nil {
			return
		}
		// Bound both connection initialization and the subscription ACK. Receive
		// without an explicit window can block indefinitely on a silent Redis.
		pubsub := subscriberClient.Subscribe(s.ctx, openAIAccountHealthSharedChannel)
		if _, err := pubsub.ReceiveTimeout(s.ctx, openAIAccountHealthIOTimeout); err != nil {
			_ = pubsub.Close()
			if s.ctx.Err() != nil {
				return
			}
			slog.Debug("openai shared health subscribe failed", "error", err)
			if !waitOpenAISharedHealthRetry(s.ctx, time.Second) {
				return
			}
			continue
		}
		messages := pubsub.Channel()
		s.refreshActiveKeys()
		for {
			select {
			case <-s.ctx.Done():
				_ = pubsub.Close()
				return
			case <-ticker.C:
				// PubSub is lossy and go-redis may reconnect internally without
				// closing Channel. Bounded refresh also repairs those missed events.
				s.refreshActiveKeys()
			case message, ok := <-messages:
				if !ok {
					_ = pubsub.Close()
					if !waitOpenAISharedHealthRetry(s.ctx, time.Second) {
						return
					}
					continue subscribeLoop
				}
				var event openAIAccountHealthSharedEvent
				if err := json.Unmarshal([]byte(message.Payload), &event); err != nil {
					continue
				}
				s.stats.applySharedHealthEvent(event)
			}
		}
	}
}

func waitOpenAISharedHealthRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *openAIAccountHealthSharedState) persist(report openAIAccountHealthSharedReport) (openAIAccountHealthSharedEvent, error) {
	errorSample := 1
	if report.success {
		errorSample = 0
	}
	ttft := ""
	if report.ttftMS != nil {
		ttft = strconv.Itoa(*report.ttftMS)
	}
	ctx, cancel := context.WithTimeout(s.ctx, openAIAccountHealthIOTimeout)
	defer cancel()
	receiptKey := ""
	if report.reportID != "" {
		receiptKey = "sub2api:openai:health-report:" + report.reportID
	}
	result, err := openAIAccountHealthUpdateScript.Run(
		ctx,
		// The shared client leaves ContextTimeoutEnabled off. Bound socket I/O
		// on this clone too, without changing the scheduling/billing client.
		s.client.WithTimeout(openAIAccountHealthIOTimeout),
		[]string{openAIAccountHealthRedisKey(report.key), openAIAccountHealthResetRedisKey(report.key.accountID), receiptKey},
		errorSample,
		ttft,
		report.updatedNano,
		int64(openAIAccountHealthSharedTTL/time.Second),
	).Result()
	if err != nil {
		return openAIAccountHealthSharedEvent{}, err
	}
	values, ok := result.([]any)
	if !ok || len(values) < 6 {
		return openAIAccountHealthSharedEvent{}, fmt.Errorf("unexpected shared health result %T", result)
	}
	return openAIAccountHealthEventFromValues(report.key, values)
}

func (s *openAIAccountHealthSharedState) load(key openAIAccountRuntimeStatsKey) {
	if s == nil || s.ctx == nil || s.ctx.Err() != nil {
		return
	}
	key = normalizeOpenAIAccountRuntimeStatsKey(key)
	redisKey := openAIAccountHealthRedisKey(key)
	if _, loaded := s.loadOnce.LoadOrStore(redisKey, struct{}{}); loaded {
		return
	}
	select {
	case <-s.ctx.Done():
		s.loadOnce.Delete(redisKey)
	case s.loads <- key:
	default:
		s.loadOnce.Delete(redisKey)
		slog.Debug("openai shared health load queue full")
	}
}

func (s *openAIAccountHealthSharedState) runLoader() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case key := <-s.loads:
			s.loadFromRedis(key)
		}
	}
}

func (s *openAIAccountHealthSharedState) loadFromRedis(key openAIAccountRuntimeStatsKey) {
	redisKey := openAIAccountHealthRedisKey(key)
	defer s.loadOnce.Delete(redisKey)
	ctx, cancel := context.WithTimeout(s.ctx, openAIAccountHealthIOTimeout)
	pipe := s.client.WithTimeout(openAIAccountHealthIOTimeout).Pipeline()
	fence := pipe.Get(ctx, openAIAccountHealthResetRedisKey(key.accountID))
	health := pipe.HGetAll(ctx, redisKey)
	_, err := pipe.Exec(ctx)
	cancel()
	if err != nil && err != redis.Nil {
		s.loadErrors.Add(1)
		return
	}
	// A reset publication can also be missed. Apply its monotonic fence before
	// the health hash, without clearing newer failures or the TTFT history.
	if resetNano, err := fence.Int64(); err == nil && resetNano > s.stats.errorResetNano(key.accountID) {
		s.stats.resetLocalErrorHealth(key.accountID, resetNano)
	}
	values, err := health.Result()
	if err != nil {
		s.loadErrors.Add(1)
		return
	}
	if len(values) == 0 {
		return
	}
	event, err := openAIAccountHealthEventFromMap(key, values)
	if err == nil {
		s.stats.applySharedHealthEvent(event)
	} else {
		s.loadErrors.Add(1)
	}
}

func (s *openAIAccountHealthSharedState) loadMissing(key openAIAccountRuntimeStatsKey) {
	if s == nil || s.ctx.Err() != nil {
		return
	}
	key = normalizeOpenAIAccountRuntimeStatsKey(key)
	now := time.Now().UnixNano()
	s.missingMu.Lock()
	if now < s.missingUntil[key] {
		s.missingMu.Unlock()
		return
	}
	if len(s.missingUntil) >= openAIAccountHealthMissingKeyLimit {
		// Never allocate a runtime stat per candidate for arbitrary model names.
		// The negative-load throttle and its cleanup work are both bounded.
		visited := 0
		for oldKey, until := range s.missingUntil {
			if until <= now {
				delete(s.missingUntil, oldKey)
			}
			visited++
			if visited >= 16 {
				break
			}
		}
		if len(s.missingUntil) >= openAIAccountHealthMissingKeyLimit {
			s.missingMu.Unlock()
			return
		}
	}
	s.missingUntil[key] = now + int64(openAIAccountHealthRefreshInterval)
	s.missingMu.Unlock()
	s.load(key)
}

func (s *openAIAccountHealthSharedState) refreshKey(key openAIAccountRuntimeStatsKey, stat *openAIAccountRuntimeStat) {
	if s == nil || stat == nil || s.ctx.Err() != nil {
		return
	}
	now := time.Now().UnixNano()
	next := stat.sharedRefreshAfterNano.Load()
	if now < next || !stat.sharedRefreshAfterNano.CompareAndSwap(next, now+int64(openAIAccountHealthRefreshInterval)) {
		return
	}
	s.load(key)
}

func (s *openAIAccountHealthSharedState) refreshActiveKeys() {
	// Bound both the scan and queued work. Request-path reads also refresh
	// their own keys asynchronously, so a large catalog cannot starve hot keys.
	visited := 0
	s.stats.accounts.Range(func(rawKey, rawStat any) bool {
		visited++
		key, keyOK := rawKey.(openAIAccountRuntimeStatsKey)
		stat, statOK := rawStat.(*openAIAccountRuntimeStat)
		if keyOK && statOK {
			s.refreshKey(key, stat)
		}
		return visited < openAIAccountHealthRefreshBatch
	})
}

func (s *openAIAccountRuntimeStats) applySharedHealthEvent(event openAIAccountHealthSharedEvent) {
	if s == nil || event.AccountID <= 0 {
		return
	}
	if event.ResetError {
		s.resetLocalErrorHealth(event.AccountID, event.UpdatedNano)
		return
	}
	if event.Version <= 0 {
		return
	}
	key := normalizeOpenAIAccountRuntimeStatsKey(openAIAccountRuntimeStatsKey{
		accountID: event.AccountID,
		kind:      event.Kind,
		model:     event.Model,
		transport: event.Transport,
	})
	stat := s.loadOrCreateForKey(key)
	if stat == nil {
		return
	}
	stat.mu.Lock()
	defer stat.mu.Unlock()
	lastUpdatedNano := stat.lastUpdatedNano.Load()
	// Expired/evicted hashes start their version counter over. A strictly newer
	// sample may establish that new sequence; delayed events from the old hash
	// still cannot overwrite a newer local or shared observation.
	if event.UpdatedNano < lastUpdatedNano ||
		(event.Version <= stat.sharedVersion.Load() && event.UpdatedNano <= lastUpdatedNano) {
		return
	}
	errorUpdatedNano := event.ErrorUpdatedNano
	if errorUpdatedNano <= 0 && event.SampleCount > 0 {
		// Events published by a replica from before the split timestamps were
		// introduced still carry UpdatedNano. Preserve health during rolling upgrades.
		errorUpdatedNano = event.UpdatedNano
	}
	if errorUpdatedNano > s.errorResetNano(event.AccountID) {
		stat.errorRateEWMABits.Store(math.Float64bits(clamp01(event.ErrorRate)))
		stat.sampleCount.Store(event.SampleCount)
		stat.errorUpdatedNano.Store(errorUpdatedNano)
	}
	if event.HasTTFT {
		stat.ttftEWMABits.Store(math.Float64bits(event.TTFT))
	} else {
		stat.ttftEWMABits.Store(math.Float64bits(math.NaN()))
	}
	stat.ttftSampleCount.Store(event.TTFTSampleCount)
	stat.lastUpdatedNano.Store(event.UpdatedNano)
	stat.ttftUpdatedNano.Store(event.TTFTUpdatedNano)
	stat.sharedVersion.Store(event.Version)
}

func (s *openAIAccountRuntimeStats) resetErrorHealth(ctx context.Context, accountID int64) error {
	if s == nil || accountID <= 0 {
		return nil
	}
	resetNano := time.Now().UnixNano()
	s.resetLocalErrorHealth(accountID, resetNano)
	shared := s.sharedHealthState()
	if shared == nil || shared.client == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	resetCtx, cancel := context.WithTimeout(ctx, openAIAccountStateUpdateTimeout)
	defer cancel()
	return shared.resetAccountErrorHealth(resetCtx, accountID, resetNano)
}

func (s *openAIAccountRuntimeStats) resetLocalErrorHealth(accountID, resetNano int64) {
	s.storeErrorResetNano(accountID, resetNano)
	s.accounts.Range(func(rawKey, value any) bool {
		key, keyOK := rawKey.(openAIAccountRuntimeStatsKey)
		stat, statOK := value.(*openAIAccountRuntimeStat)
		if !keyOK || !statOK || key.accountID != accountID {
			return true
		}
		stat.mu.Lock()
		if stat.errorUpdatedNano.Load() <= resetNano {
			stat.errorRateEWMABits.Store(math.Float64bits(0))
			stat.sampleCount.Store(0)
			stat.errorUpdatedNano.Store(0)
		}
		stat.mu.Unlock()
		return true
	})
}

func (s *openAIAccountRuntimeStats) storeErrorResetNano(accountID, resetNano int64) {
	if s == nil || accountID <= 0 || resetNano <= 0 {
		return
	}
	value, _ := s.errorResetAt.LoadOrStore(accountID, &atomic.Int64{})
	resetAt, _ := value.(*atomic.Int64)
	if resetAt == nil {
		return
	}
	for current := resetAt.Load(); resetNano > current; current = resetAt.Load() {
		if resetAt.CompareAndSwap(current, resetNano) {
			return
		}
	}
}

func (s *openAIAccountRuntimeStats) errorResetNano(accountID int64) int64 {
	if s == nil || accountID <= 0 {
		return 0
	}
	value, ok := s.errorResetAt.Load(accountID)
	if !ok {
		return 0
	}
	resetAt, _ := value.(*atomic.Int64)
	if resetAt == nil {
		return 0
	}
	return resetAt.Load()
}

func (s *openAIAccountHealthSharedState) resetAccountErrorHealth(ctx context.Context, accountID, resetNano int64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || s.client == nil || accountID <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, openAIAccountHealthIOTimeout)
	defer cancel()
	clientForIO := func() (*redis.Client, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		timeout := openAIAccountHealthIOTimeout
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, context.DeadlineExceeded
			}
			if remaining < timeout {
				timeout = remaining
			}
		}
		return s.client.WithTimeout(timeout), nil
	}
	redisClient, err := clientForIO()
	if err != nil {
		return err
	}
	effectiveResetNano, err := openAIAccountHealthResetFenceScript.Run(
		ctx,
		redisClient,
		[]string{openAIAccountHealthResetRedisKey(accountID)},
		resetNano,
		int64(openAIAccountHealthSharedTTL/time.Second),
	).Int64()
	if err != nil {
		return err
	}
	var cursor uint64
	pattern := openAIAccountHealthSharedKeyPrefix + strconv.FormatInt(accountID, 10) + ":*"
	for {
		redisClient, err = clientForIO()
		if err != nil {
			return err
		}
		keys, next, err := redisClient.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return err
		}
		for _, redisKey := range keys {
			redisClient, err = clientForIO()
			if err != nil {
				return err
			}
			changed, err := openAIAccountHealthResetErrorScript.Run(ctx, redisClient, []string{redisKey}, effectiveResetNano, int64(openAIAccountHealthSharedTTL/time.Second)).Int()
			if err != nil {
				return err
			}
			_ = changed
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	event := openAIAccountHealthSharedEvent{AccountID: accountID, ResetError: true, UpdatedNano: effectiveResetNano}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	redisClient, err = clientForIO()
	if err != nil {
		return err
	}
	return redisClient.Publish(ctx, openAIAccountHealthSharedChannel, payload).Err()
}

func openAIAccountHealthRedisKey(key openAIAccountRuntimeStatsKey) string {
	key = normalizeOpenAIAccountRuntimeStatsKey(key)
	sum := sha256.Sum256([]byte(key.kind + "\x00" + key.model + "\x00" + key.transport))
	return openAIAccountHealthSharedKeyPrefix + strconv.FormatInt(key.accountID, 10) + ":" + hex.EncodeToString(sum[:])
}

func openAIAccountHealthResetRedisKey(accountID int64) string {
	return openAIAccountHealthResetKeyPrefix + strconv.FormatInt(accountID, 10)
}

func openAIAccountHealthEventFromValues(key openAIAccountRuntimeStatsKey, values []any) (openAIAccountHealthSharedEvent, error) {
	errorRate, err := strconv.ParseFloat(fmt.Sprint(values[0]), 64)
	if err != nil {
		return openAIAccountHealthSharedEvent{}, err
	}
	ttftRaw := fmt.Sprint(values[1])
	ttft := 0.0
	hasTTFT := ttftRaw != ""
	if hasTTFT {
		ttft, err = strconv.ParseFloat(ttftRaw, 64)
		if err != nil {
			return openAIAccountHealthSharedEvent{}, err
		}
	}
	sampleCount, err := strconv.ParseInt(fmt.Sprint(values[2]), 10, 64)
	if err != nil {
		return openAIAccountHealthSharedEvent{}, err
	}
	ttftSampleCount, err := strconv.ParseInt(fmt.Sprint(values[3]), 10, 64)
	if err != nil {
		return openAIAccountHealthSharedEvent{}, err
	}
	updatedNano, err := strconv.ParseInt(fmt.Sprint(values[4]), 10, 64)
	if err != nil {
		return openAIAccountHealthSharedEvent{}, err
	}
	version, err := strconv.ParseInt(fmt.Sprint(values[5]), 10, 64)
	if err != nil {
		return openAIAccountHealthSharedEvent{}, err
	}
	errorUpdatedNano := updatedNano
	if len(values) > 6 && fmt.Sprint(values[6]) != "" {
		errorUpdatedNano, err = strconv.ParseInt(fmt.Sprint(values[6]), 10, 64)
		if err != nil {
			return openAIAccountHealthSharedEvent{}, err
		}
	}
	ttftUpdatedNano := int64(0)
	if len(values) > 7 && fmt.Sprint(values[7]) != "" {
		ttftUpdatedNano, err = strconv.ParseInt(fmt.Sprint(values[7]), 10, 64)
		if err != nil {
			return openAIAccountHealthSharedEvent{}, err
		}
	} else if hasTTFT {
		ttftUpdatedNano = updatedNano
	}
	key = normalizeOpenAIAccountRuntimeStatsKey(key)
	return openAIAccountHealthSharedEvent{
		AccountID:        key.accountID,
		Kind:             key.kind,
		Model:            key.model,
		Transport:        key.transport,
		ErrorRate:        errorRate,
		TTFT:             ttft,
		HasTTFT:          hasTTFT,
		SampleCount:      sampleCount,
		TTFTSampleCount:  ttftSampleCount,
		UpdatedNano:      updatedNano,
		ErrorUpdatedNano: errorUpdatedNano,
		TTFTUpdatedNano:  ttftUpdatedNano,
		Version:          version,
	}, nil
}

func openAIAccountHealthEventFromMap(key openAIAccountRuntimeStatsKey, values map[string]string) (openAIAccountHealthSharedEvent, error) {
	return openAIAccountHealthEventFromValues(key, []any{
		values["error_rate"],
		values["ttft"],
		values["sample_count"],
		values["ttft_sample_count"],
		values["updated_ns"],
		values["version"],
		values["error_updated_ns"],
		values["ttft_updated_ns"],
	})
}
