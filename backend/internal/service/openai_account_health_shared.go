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
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	openAIAccountHealthSharedChannel   = "sub2api:openai:health:v1"
	openAIAccountHealthSharedKeyPrefix = "sub2api:openai:health:v1:"
	openAIAccountHealthSharedTTL       = 10 * time.Minute
	openAIAccountHealthSharedQueueSize = 4096
	openAIAccountHealthLoadQueueSize   = 1024
	openAIAccountHealthLoadWorkers     = 2
)

var openAIAccountHealthUpdateScript = redis.NewScript(`
local error_sample = tonumber(ARGV[1])
local ttft_sample = ARGV[2]
local updated_ns = ARGV[3]
local ttl_seconds = tonumber(ARGV[4])
local alpha = 0.2

local old_error = redis.call('HGET', KEYS[1], 'error_rate')
local error_rate = error_sample
if old_error then
  error_rate = alpha * error_sample + (1 - alpha) * tonumber(old_error)
end

local sample_count = redis.call('HINCRBY', KEYS[1], 'sample_count', 1)
local ttft_sample_count = tonumber(redis.call('HGET', KEYS[1], 'ttft_sample_count') or '0')
local ttft = redis.call('HGET', KEYS[1], 'ttft')
if ttft_sample ~= '' then
  local sample = tonumber(ttft_sample)
  if ttft then
    ttft = alpha * sample + (1 - alpha) * tonumber(ttft)
  else
    ttft = sample
  end
  ttft_sample_count = redis.call('HINCRBY', KEYS[1], 'ttft_sample_count', 1)
end

local version = redis.call('HINCRBY', KEYS[1], 'version', 1)
redis.call('HSET', KEYS[1],
  'error_rate', tostring(error_rate),
  'updated_ns', updated_ns,
  'ttft_sample_count', tostring(ttft_sample_count))
if ttft then
  redis.call('HSET', KEYS[1], 'ttft', tostring(ttft))
end
redis.call('EXPIRE', KEYS[1], ttl_seconds)

return {tostring(error_rate), ttft or '', sample_count, ttft_sample_count, updated_ns, version}
`)

type openAIAccountHealthSharedReport struct {
	key         openAIAccountRuntimeStatsKey
	success     bool
	ttftMS      *int
	updatedNano int64
}

type openAIAccountHealthSharedEvent struct {
	AccountID       int64   `json:"account_id"`
	Kind            string  `json:"kind"`
	Model           string  `json:"model"`
	Transport       string  `json:"transport"`
	ErrorRate       float64 `json:"error_rate"`
	TTFT            float64 `json:"ttft"`
	HasTTFT         bool    `json:"has_ttft"`
	SampleCount     int64   `json:"sample_count"`
	TTFTSampleCount int64   `json:"ttft_sample_count"`
	UpdatedNano     int64   `json:"updated_nano"`
	Version         int64   `json:"version"`
}

type openAIAccountHealthSharedState struct {
	client      *redis.Client
	stats       *openAIAccountRuntimeStats
	reports     chan openAIAccountHealthSharedReport
	loads       chan openAIAccountRuntimeStatsKey
	ctx         context.Context
	cancel      context.CancelFunc
	lifecycleMu sync.Mutex
	closed      bool
	startOnce   sync.Once
	closeOnce   sync.Once
	workers     sync.WaitGroup
	loadOnce    sync.Map
}

func newOpenAIAccountHealthSharedState(client *redis.Client, stats *openAIAccountRuntimeStats, queueSize int) *openAIAccountHealthSharedState {
	if queueSize <= 0 {
		queueSize = openAIAccountHealthSharedQueueSize
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &openAIAccountHealthSharedState{
		client:  client,
		stats:   stats,
		reports: make(chan openAIAccountHealthSharedReport, queueSize),
		loads:   make(chan openAIAccountRuntimeStatsKey, openAIAccountHealthLoadQueueSize),
		ctx:     ctx,
		cancel:  cancel,
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
	report := openAIAccountHealthSharedReport{key: key, success: success, updatedNano: updatedNano}
	if firstTokenMS != nil && *firstTokenMS > 0 {
		value := *firstTokenMS
		report.ttftMS = &value
	}
	select {
	case shared.reports <- report:
	default:
		slog.Debug("openai shared health report queue full")
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
		event, err := s.persist(report)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			slog.Debug("openai shared health update failed", "error", err)
			continue
		}
		s.stats.applySharedHealthEvent(event)
		payload, err := json.Marshal(event)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
		err = s.client.Publish(ctx, openAIAccountHealthSharedChannel, payload).Err()
		cancel()
		if err != nil {
			slog.Debug("openai shared health publish failed", "error", err)
		}
	}
}

func (s *openAIAccountHealthSharedState) runSubscriber() {
subscribeLoop:
	for {
		if s.ctx.Err() != nil {
			return
		}
		pubsub := s.client.Subscribe(s.ctx, openAIAccountHealthSharedChannel)
		if _, err := pubsub.Receive(s.ctx); err != nil {
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
		for {
			select {
			case <-s.ctx.Done():
				_ = pubsub.Close()
				return
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
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	result, err := openAIAccountHealthUpdateScript.Run(
		ctx,
		s.client,
		[]string{openAIAccountHealthRedisKey(report.key)},
		errorSample,
		ttft,
		report.updatedNano,
		int64(openAIAccountHealthSharedTTL/time.Second),
	).Result()
	if err != nil {
		return openAIAccountHealthSharedEvent{}, err
	}
	values, ok := result.([]any)
	if !ok || len(values) != 6 {
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
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	values, err := s.client.HGetAll(ctx, redisKey).Result()
	cancel()
	if err != nil {
		s.loadOnce.Delete(redisKey)
		return
	}
	if len(values) == 0 {
		return
	}
	event, err := openAIAccountHealthEventFromMap(key, values)
	if err == nil {
		s.stats.applySharedHealthEvent(event)
	}
}

func (s *openAIAccountRuntimeStats) applySharedHealthEvent(event openAIAccountHealthSharedEvent) {
	if s == nil || event.AccountID <= 0 || event.Version <= 0 {
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
	if event.Version <= stat.sharedVersion.Load() || event.UpdatedNano < stat.lastUpdatedNano.Load() {
		return
	}
	stat.errorRateEWMABits.Store(math.Float64bits(clamp01(event.ErrorRate)))
	if event.HasTTFT {
		stat.ttftEWMABits.Store(math.Float64bits(event.TTFT))
	} else {
		stat.ttftEWMABits.Store(math.Float64bits(math.NaN()))
	}
	stat.sampleCount.Store(event.SampleCount)
	stat.ttftSampleCount.Store(event.TTFTSampleCount)
	stat.lastUpdatedNano.Store(event.UpdatedNano)
	stat.sharedVersion.Store(event.Version)
}

func openAIAccountHealthRedisKey(key openAIAccountRuntimeStatsKey) string {
	key = normalizeOpenAIAccountRuntimeStatsKey(key)
	sum := sha256.Sum256([]byte(key.kind + "\x00" + key.model + "\x00" + key.transport))
	return openAIAccountHealthSharedKeyPrefix + strconv.FormatInt(key.accountID, 10) + ":" + hex.EncodeToString(sum[:])
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
	key = normalizeOpenAIAccountRuntimeStatsKey(key)
	return openAIAccountHealthSharedEvent{
		AccountID:       key.accountID,
		Kind:            key.kind,
		Model:           key.model,
		Transport:       key.transport,
		ErrorRate:       errorRate,
		TTFT:            ttft,
		HasTTFT:         hasTTFT,
		SampleCount:     sampleCount,
		TTFTSampleCount: ttftSampleCount,
		UpdatedNano:     updatedNano,
		Version:         version,
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
	})
}
