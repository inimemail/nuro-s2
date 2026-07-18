package securityaudit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
)

const (
	maxWorkers         = 16
	payloadTTL         = 10 * time.Minute
	cleanupInterval    = 6 * time.Hour
	workerTaskTimeout  = 45 * time.Second
	recoveryInterval   = 30 * time.Second
	recoveryStaleAfter = 2 * time.Minute
	recoveryTimeout    = 15 * time.Second
	recoveryBatchSize  = 64
	configRefreshEvery = 30 * time.Second
	shutdownDrainGrace = 5 * time.Second
)

type auditTask struct {
	request       Request
	jobID         int64
	text          string
	hash          string
	preview       string
	promptLength  int
	messageCount  int
	configVersion int64
	recovered     bool
}

type Service struct {
	settingRepo service.SettingRepository
	db          *sql.DB
	redis       *redis.Client
	encryptor   service.SecretEncryptor

	config         atomic.Pointer[Config]
	featureEnabled atomic.Bool
	queue          chan auditTask

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	start  sync.Once
	stop   sync.Once

	configSignalMu sync.Mutex
	configSignal   chan struct{}
	saveMu         sync.Mutex

	hashKey     atomic.Pointer[[32]byte]
	stopping    atomic.Bool
	active      atomic.Int64
	queued      atomic.Int64
	queuedBytes atomic.Int64
	enqueued    atomic.Int64
	dropped     atomic.Int64
	processed   atomic.Int64
	failed      atomic.Int64
	lastError   atomic.Value
}

func NewService(settingRepo service.SettingRepository, db *sql.DB, redisClient *redis.Client, encryptor service.SecretEncryptor) *Service {
	svc := &Service{
		settingRepo: settingRepo, db: db, redis: redisClient, encryptor: encryptor,
		queue: make(chan auditTask, 100000), configSignal: make(chan struct{}),
	}
	svc.lastError.Store("")
	hashKey := new([32]byte)
	if _, err := rand.Read(hashKey[:]); err != nil {
		slog.Warn("prompt_audit_hash_secret_unavailable", "error_code", "random_source_unavailable")
	} else {
		svc.hashKey.Store(hashKey)
	}
	cfg := DefaultConfig()
	svc.storeConfig(cfg)
	return svc
}

func (s *Service) Start(parent context.Context) {
	if s == nil {
		return
	}
	s.start.Do(func() {
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithCancel(parent)
		s.ctx, s.cancel = ctx, cancel
		loadCtx, loadCancel := context.WithTimeout(ctx, 5*time.Second)
		if err := s.loadConfig(loadCtx); err != nil {
			slog.Warn("prompt_audit_config_load_failed", "error_code", "config_load_failed")
			s.lastError.Store("config_load_failed")
			cfg := DefaultConfig()
			s.storeConfig(cfg)
		}
		loadCancel()
		for workerID := 0; workerID < maxWorkers; workerID++ {
			s.wg.Add(1)
			go s.worker(ctx, workerID)
		}
		s.wg.Add(1)
		go s.cleanupLoop(ctx)
		s.wg.Add(1)
		go s.recoveryLoop(ctx)
		s.wg.Add(1)
		go s.configRefreshLoop(ctx)
	})
}

func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.stop.Do(func() {
		s.stopping.Store(true)
		if s.cancel != nil {
			s.waitForDrain()
		}
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()
		s.discardQueuedTasks()
	})
}

func (s *Service) waitForDrain() {
	deadline := time.NewTimer(shutdownDrainGrace)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for s.queued.Load() > 0 || s.active.Load() > 0 {
		select {
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) EnabledFast() bool {
	if s == nil || !s.featureEnabled.Load() {
		return false
	}
	cfg := s.config.Load()
	if cfg == nil || !cfg.Enabled || cfg.Mode != ModeAsync {
		return false
	}
	for i := range cfg.Endpoints {
		if cfg.Endpoints[i].Enabled {
			return true
		}
	}
	return false
}

// SetFeatureEnabled updates the system-level Prompt Audit gate. It is called
// after startup loading and immediately after an admin settings update.
func (s *Service) SetFeatureEnabled(enabled bool) {
	if s != nil {
		s.featureEnabled.Store(enabled)
	}
}

// FeatureEnabledFast reports only the system-level gate state. It deliberately
// avoids settings I/O so both admin routing and gateway collection fail closed.
func (s *Service) FeatureEnabledFast() bool {
	return s != nil && s.featureEnabled.Load()
}

func (s *Service) NewCollector() *Collector {
	if !s.EnabledFast() {
		return nil
	}
	return &Collector{owner: s}
}

// FlushCollector is called only after the handler has returned. Body copies,
// queue admission and every DB/Redis operation therefore happen outside the
// first-token path.
func (s *Service) FlushCollector(collector *Collector) {
	if s == nil || collector == nil || !s.EnabledFast() {
		return
	}
	s.flushRequests(collector.take())
}

func (s *Service) flushRequests(requests []Request) {
	if s == nil || !s.EnabledFast() {
		return
	}
	cfg := s.configSnapshot()
	for _, req := range requests {
		if !cfg.includesGroup(req.GroupID) || (len(req.Body) == 0 && len(req.PromptTexts) == 0 && req.CaptureError == "") {
			continue
		}
		cloned := req.cloneBody()
		s.enqueueTask(auditTask{request: cloned}, cfg)
	}
}

func (s *Service) enqueueTask(task auditTask, cfg Config) bool {
	if s == nil {
		return false
	}
	if s.stopping.Load() {
		s.dropped.Add(1)
		return false
	}
	size := taskQueueBytes(task)
	bytesReserved := false
	for !bytesReserved {
		current := s.queuedBytes.Load()
		if current+size > MaxQueuedBytes {
			s.dropped.Add(1)
			return false
		}
		bytesReserved = s.queuedBytes.CompareAndSwap(current, current+size)
	}
	reserved := false
	for !reserved {
		queued := s.queued.Load()
		if queued >= int64(cfg.QueueCapacity) {
			s.queuedBytes.Add(-size)
			s.dropped.Add(1)
			return false
		}
		reserved = s.queued.CompareAndSwap(queued, queued+1)
	}
	select {
	case s.queue <- task:
		s.enqueued.Add(1)
		return true
	default:
		s.queued.Add(-1)
		s.queuedBytes.Add(-size)
		s.dropped.Add(1)
		return false
	}
}

// Requests that have not yet reached createJob are best-effort. Shutdown
// drains them for a bounded grace period, then explicitly releases remaining
// prompt buffers instead of retaining them in a stopped service.
func (s *Service) discardQueuedTasks() {
	for {
		select {
		case task := <-s.queue:
			s.queued.Add(-1)
			s.queuedBytes.Add(-taskQueueBytes(task))
			s.dropped.Add(1)
		default:
			return
		}
	}
}

func (s *Service) worker(ctx context.Context, workerID int) {
	defer s.wg.Done()
	for {
		configSignal := s.configSignalSnapshot()
		if workerID >= s.configSnapshot().WorkerCount {
			select {
			case <-ctx.Done():
				return
			case <-configSignal:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-configSignal:
			continue
		case task := <-s.queue:
			if workerID >= s.configSnapshot().WorkerCount {
				select {
				case s.queue <- task:
				case <-ctx.Done():
					return
				}
				continue
			}
			s.active.Add(1)
			s.queued.Add(-1)
			s.queuedBytes.Add(-taskQueueBytes(task))
			func() {
				defer s.active.Add(-1)
				defer func() {
					if recover() != nil {
						s.fail("worker_panic")
					}
				}()
				s.processTask(ctx, task)
			}()
		}
	}
}

func (s *Service) processTask(parent context.Context, task auditTask) {
	if !s.FeatureEnabledFast() {
		return
	}
	cfg := s.configSnapshot()
	if !cfg.Enabled || cfg.Mode != ModeAsync {
		return
	}
	if !cfg.includesGroup(task.request.GroupID) {
		if task.recovered && task.jobID > 0 {
			ctx, cancel := context.WithTimeout(parent, recoveryTimeout)
			_ = s.updateJobStatus(ctx, task.jobID, "failed", "config_scope_changed")
			cancel()
		}
		return
	}
	if task.request.CaptureError != "" {
		s.processCaptureError(parent, task.request, cfg)
		return
	}
	text := task.text
	messageCount := task.messageCount
	hash := task.hash
	preview := task.preview
	promptLength := task.promptLength
	configVersion := task.configVersion
	jobID := task.jobID
	if !task.recovered {
		text, messageCount = extractAuditPrompt(task.request)
		if text == "" {
			return
		}
		hash = s.promptHash(text)
		preview = redactPreview(text)
		promptLength = len([]rune(text))
		configVersion = cfg.Version
	} else if text == "" || jobID <= 0 {
		s.fail("recovery_payload_invalid")
		return
	}
	if configVersion <= 0 {
		configVersion = cfg.Version
	}
	ctx, cancel := context.WithTimeout(parent, workerTaskTimeout)
	defer cancel()
	if !task.recovered {
		var err error
		jobID, err = s.createJob(ctx, task.request, hash, preview, promptLength, messageCount, configVersion)
		if err != nil {
			s.fail("job_create_failed")
			return
		}
	}
	if s.encryptor == nil || s.redis == nil {
		_ = s.updateJobStatus(ctx, jobID, "failed", "payload_store_unavailable")
		s.fail("payload_store_unavailable")
		return
	}
	payloadKey := "sub2api:prompt_audit:payload:" + formatInt64(jobID)
	if !task.recovered {
		ciphertext, err := s.encryptor.Encrypt(text)
		if err != nil {
			_ = s.updateJobStatus(ctx, jobID, "failed", "payload_encrypt_failed")
			s.fail("payload_encrypt_failed")
			return
		}
		if err := s.redis.Set(ctx, payloadKey, ciphertext, payloadTTL).Err(); err != nil {
			_ = s.updateJobStatus(ctx, jobID, "failed", "payload_store_failed")
			s.fail("payload_store_failed")
			return
		}
	}
	deletePayload := false
	defer func() {
		if !deletePayload {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.redis.Del(cleanupCtx, payloadKey).Err()
		cleanupCancel()
	}()
	if err := s.updateJobStatus(ctx, jobID, "processing", ""); err != nil {
		s.fail("job_state_failed")
		return
	}
	result, scanErr := s.scan(ctx, cfg, text)
	if scanErr != nil {
		result = scanResult{
			Decision: "unavailable", Risk: "unknown", Action: "Observe",
			Backend: "qwen3guard-openai", Categories: []string{},
		}
		if err := s.createEvent(ctx, jobID, task.request, hash, preview, promptLength, messageCount, result, configVersion, "guard_unavailable"); err != nil {
			_ = s.updateJobStatus(ctx, jobID, "retry", "event_write_failed")
			s.fail("event_write_failed")
			return
		}
		if err := s.updateJobStatus(ctx, jobID, "failed", "guard_unavailable"); err == nil {
			deletePayload = true
		}
		s.fail("guard_unavailable")
		return
	}
	if cfg.StorePass || result.Decision != "pass" {
		if err := s.createEvent(ctx, jobID, task.request, hash, preview, promptLength, messageCount, result, configVersion, ""); err != nil {
			_ = s.updateJobStatus(ctx, jobID, "retry", "event_write_failed")
			s.fail("event_write_failed")
			return
		}
	}
	if err := s.updateJobStatus(ctx, jobID, "done", ""); err != nil {
		s.fail("job_state_failed")
		return
	}
	deletePayload = true
	s.processed.Add(1)
}

func (s *Service) promptHash(text string) string {
	if s == nil {
		return ""
	}
	key := s.hashKey.Load()
	if key == nil {
		return ""
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(text))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) processCaptureError(parent context.Context, req Request, cfg Config) {
	ctx, cancel := context.WithTimeout(parent, workerTaskTimeout)
	defer cancel()
	jobID, err := s.createJob(ctx, req, "", "", 0, 0, cfg.Version)
	if err != nil {
		s.fail("job_create_failed")
		return
	}
	result := scanResult{Decision: "unavailable", Risk: "unknown", Action: "Observe", Backend: "local", Categories: []string{}}
	if err := s.createEvent(ctx, jobID, req, "", "", 0, 0, result, cfg.Version, req.CaptureError); err != nil {
		_ = s.updateJobStatus(ctx, jobID, "failed", "event_write_failed")
		s.fail("event_write_failed")
		return
	}
	if err := s.updateJobStatus(ctx, jobID, "done", ""); err != nil {
		s.fail("job_state_failed")
		return
	}
	s.processed.Add(1)
}

func (s *Service) fail(code string) {
	s.failed.Add(1)
	s.lastError.Store(code)
	slog.Warn("prompt_audit_background_failed", "error_code", code)
}

func (s *Service) cleanupLoop(ctx context.Context) {
	defer s.wg.Done()
	s.runCleanup(ctx)
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runCleanup(ctx)
		}
	}
}

func (s *Service) runCleanup(parent context.Context) {
	if !s.FeatureEnabledFast() {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if err := s.cleanupExpired(cleanupCtx, s.configSnapshot().RetentionDays); err != nil {
		s.fail("cleanup_failed")
	}
}

func (s *Service) recoveryLoop(ctx context.Context) {
	defer s.wg.Done()
	s.recoverPendingJobs(ctx)
	ticker := time.NewTicker(recoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.recoverPendingJobs(ctx)
		}
	}
}

func (s *Service) configRefreshLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(configRefreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := s.refreshConfig(refreshCtx); err != nil {
				s.lastError.Store("config_refresh_failed")
				slog.Warn("prompt_audit_config_refresh_failed", "error_code", "config_refresh_failed")
			}
			cancel()
		}
	}
}

func (s *Service) recoverPendingJobs(parent context.Context) {
	if s == nil || !s.EnabledFast() || s.db == nil || s.redis == nil || s.encryptor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, recoveryTimeout)
	defer cancel()
	jobs, err := s.claimRecoverableJobs(ctx, time.Now().UTC().Add(-recoveryStaleAfter), recoveryBatchSize)
	if err != nil {
		s.fail("recovery_claim_failed")
		return
	}
	cfg := s.configSnapshot()
	for _, job := range jobs {
		if !cfg.Enabled || cfg.Mode != ModeAsync {
			_ = s.updateJobStatus(ctx, job.ID, "retry", "audit_disabled")
			continue
		}
		if !cfg.includesGroup(job.Request.GroupID) {
			_ = s.updateJobStatus(ctx, job.ID, "failed", "config_scope_changed")
			continue
		}
		payloadKey := "sub2api:prompt_audit:payload:" + formatInt64(job.ID)
		ciphertext, err := s.redis.Get(ctx, payloadKey).Result()
		if err != nil {
			code := "recovery_payload_unavailable"
			if errors.Is(err, redis.Nil) {
				code = "recovery_payload_expired"
			}
			_ = s.updateJobStatus(ctx, job.ID, "failed", code)
			s.fail(code)
			continue
		}
		text, err := s.encryptor.Decrypt(ciphertext)
		if err != nil || strings.TrimSpace(text) == "" {
			_ = s.updateJobStatus(ctx, job.ID, "failed", "recovery_payload_invalid")
			s.fail("recovery_payload_invalid")
			continue
		}
		task := auditTask{
			request:       job.Request,
			jobID:         job.ID,
			text:          text,
			hash:          job.Hash,
			preview:       job.Preview,
			promptLength:  job.PromptLength,
			messageCount:  job.MessageCount,
			configVersion: job.ConfigVersion,
			recovered:     true,
		}
		if !s.enqueueTask(task, cfg) {
			_ = s.updateJobStatus(ctx, job.ID, "retry", "recovery_queue_full")
		}
	}
}

func taskQueueBytes(task auditTask) int64 {
	size := int64(len(task.request.Body) + len(task.text))
	for _, value := range task.request.PromptTexts {
		size += int64(len(value))
	}
	if size < 1 {
		size = 1
	}
	return size
}

func (s *Service) Runtime() Runtime {
	cfg := s.configSnapshot()
	lastError, _ := s.lastError.Load().(string)
	return Runtime{
		Mode: cfg.Mode, WorkerCount: cfg.WorkerCount, QueueCapacity: cfg.QueueCapacity,
		QueueLength: int(s.queued.Load()), Enqueued: s.enqueued.Load(), Dropped: s.dropped.Load(),
		Processed: s.processed.Load(), Failed: s.failed.Load(), LastError: lastError,
		UpdatedAt: cfg.UpdatedAt,
	}
}

func (s *Service) Probe(ctx context.Context, input UpdateEndpoint) (RuntimeProbeResult, error) {
	endpoint := EndpointConfig{
		ID: strings.TrimSpace(input.ID), Name: strings.TrimSpace(input.Name),
		BaseURL: strings.TrimSpace(input.BaseURL), Model: strings.TrimSpace(input.Model),
		TimeoutMS: input.TimeoutMS, Enabled: true, AllowPrivate: input.AllowPrivate,
		AllowedCIDRs: append([]string(nil), input.AllowedCIDRs...),
	}
	if endpoint.ID == "" {
		endpoint.ID = "probe"
	}
	if endpoint.Name == "" {
		endpoint.Name = "probe"
	}
	if endpoint.TimeoutMS <= 0 {
		endpoint.TimeoutMS = DefaultTimeoutMS
	}
	switch token := strings.TrimSpace(input.Token); {
	case input.ClearToken:
	case token != "":
		if s == nil || s.encryptor == nil {
			return RuntimeProbeResult{}, errors.New("probe token unavailable")
		}
		ciphertext, err := s.encryptor.Encrypt(token)
		if err != nil {
			return RuntimeProbeResult{}, errors.New("probe token unavailable")
		}
		endpoint.TokenCiphertext = ciphertext
	case s != nil:
		for _, configured := range s.configSnapshot().Endpoints {
			if configured.ID == endpoint.ID && endpointCredentialBindingEqual(configured, endpoint) {
				endpoint.TokenCiphertext = configured.TokenCiphertext
				break
			}
		}
	}
	if err := validateConfig(Config{
		Enabled: true, Mode: ModeAsync, WorkerCount: 1, QueueCapacity: 1,
		AllGroups: true, Scanners: append([]string(nil), defaultScanners...),
		Endpoints: []EndpointConfig{endpoint}, RetentionDays: 1, Version: 1,
	}); err != nil {
		return RuntimeProbeResult{}, err
	}
	started := time.Now()
	result, err := s.scanEndpoint(ctx, endpoint, defaultScanners, "Hello")
	if err != nil {
		return RuntimeProbeResult{OK: false, Status: "failed", ErrorCode: "endpoint_unavailable", LatencyMS: int(time.Since(started).Milliseconds()), CheckedAt: time.Now().UTC()}, nil
	}
	return RuntimeProbeResult{OK: true, Status: "healthy", LatencyMS: result.LatencyMS, CheckedAt: time.Now().UTC()}, nil
}

type RuntimeProbeResult struct {
	OK        bool      `json:"ok"`
	Status    string    `json:"status"`
	ErrorCode string    `json:"error_code,omitempty"`
	LatencyMS int       `json:"latency_ms"`
	CheckedAt time.Time `json:"checked_at"`
}

func (s *Service) storeConfig(cfg Config) {
	s.installHashKey(cfg)
	clone := cfg
	clone.GroupIDs = append([]int64(nil), cfg.GroupIDs...)
	clone.Scanners = append([]string(nil), cfg.Scanners...)
	clone.Endpoints = append([]EndpointConfig(nil), cfg.Endpoints...)
	s.config.Store(&clone)
	s.configSignalMu.Lock()
	close(s.configSignal)
	s.configSignal = make(chan struct{})
	s.configSignalMu.Unlock()
}

func (s *Service) installHashKey(cfg Config) {
	if s == nil || s.encryptor == nil || strings.TrimSpace(cfg.HashSecret) == "" {
		return
	}
	plaintext, err := s.encryptor.Decrypt(cfg.HashSecret)
	if err != nil {
		return
	}
	decoded, err := hex.DecodeString(plaintext)
	if err != nil || len(decoded) != sha256.Size {
		return
	}
	key := new([32]byte)
	copy(key[:], decoded)
	s.hashKey.Store(key)
}

func (s *Service) configSignalSnapshot() <-chan struct{} {
	s.configSignalMu.Lock()
	defer s.configSignalMu.Unlock()
	return s.configSignal
}

func (s *Service) configSnapshot() Config {
	if s == nil {
		return DefaultConfig()
	}
	cfg := s.config.Load()
	if cfg == nil {
		return DefaultConfig()
	}
	clone := *cfg
	clone.GroupIDs = append([]int64(nil), cfg.GroupIDs...)
	clone.Scanners = append([]string(nil), cfg.Scanners...)
	clone.Endpoints = append([]EndpointConfig(nil), cfg.Endpoints...)
	return clone
}

var (
	promptEmailPattern  = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	promptSecretPattern = regexp.MustCompile(`(?i)\b(?:sk|rk|pk|ghp|gho|github_pat|xox[baprs]-|AKIA|ASIA)[a-z0-9_\-]{8,}\b|\bBearer\s+[^\s]+|\beyJ[a-z0-9_\-]+\.[a-z0-9_\-]+\.[a-z0-9_\-]+\b`)
	privateKeyPattern   = regexp.MustCompile(`(?s)-----BEGIN [^-]+-----.*?-----END [^-]+-----`)
	jsonSecretPattern   = regexp.MustCompile(`(?i)("(?:api[_-]?key|x-api-key|authorization|cookie|token|secret|password|private[_-]?key|client[_-]?secret|access[_-]?key)"\s*:\s*")[^"]*(")`)
	textSecretPattern   = regexp.MustCompile(`(?i)(\b(?:api[_-]?key|x-api-key|authorization|cookie|token|secret|password|private[_-]?key|client[_-]?secret|access[_-]?key)\b\s*[:=]\s*)[^\s,;]+`)
)

func redactPreview(value string) string {
	value = sanitizePreviewText(value)
	value = privateKeyPattern.ReplaceAllString(value, "[PRIVATE_KEY]")
	value = promptEmailPattern.ReplaceAllString(value, "[EMAIL]")
	value = promptSecretPattern.ReplaceAllString(value, "[SECRET]")
	value = jsonSecretPattern.ReplaceAllString(value, "$1[REDACTED]$2")
	value = textSecretPattern.ReplaceAllString(value, "$1[REDACTED]")
	return trimPromptRunes(value, MaxPreviewRunes)
}

func sanitizePreviewText(value string) string {
	var sanitized strings.Builder
	sanitized.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			sanitized.WriteByte(' ')
		case r == 0 || r < 0x20 || r == 0x7f:
			continue
		default:
			sanitized.WriteRune(r)
		}
	}
	return sanitized.String()
}

func trimPromptRunes(value string, limit int) string {
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func promptMessageCount(body []byte) int {
	for _, path := range []string{"messages", "input", "contents", "request.items"} {
		value := gjson.GetBytes(body, path)
		if value.IsArray() {
			return len(value.Array())
		}
	}
	return 1
}

func extractAuditPrompt(req Request) (string, int) {
	if len(req.PromptTexts) > 0 {
		parts := make([]string, 0, len(req.PromptTexts))
		for _, value := range req.PromptTexts {
			if value = strings.TrimSpace(value); value != "" {
				parts = append(parts, value)
			}
		}
		return trimPromptRunes(strings.Join(parts, "\n"), MaxPromptRunes), len(parts)
	}
	if req.Protocol == "openai_embeddings" {
		parts := make([]string, 0)
		input := gjson.GetBytes(req.Body, "input")
		switch {
		case input.Type == gjson.String:
			parts = append(parts, input.String())
		case input.IsArray():
			input.ForEach(func(_, item gjson.Result) bool {
				if item.Type == gjson.String {
					parts = append(parts, item.String())
				}
				return true
			})
		}
		return trimPromptRunes(strings.TrimSpace(strings.Join(parts, "\n")), MaxPromptRunes), len(parts)
	}
	if req.Protocol == "openai_alpha_search" {
		parts := make([]string, 0, 4)
		for _, path := range []string{"input", "query", "prompt", "text"} {
			value := gjson.GetBytes(req.Body, path)
			if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
				parts = append(parts, value.String())
			}
		}
		return trimPromptRunes(strings.TrimSpace(strings.Join(parts, "\n")), MaxPromptRunes), len(parts)
	}
	return extractStructuredAuditPrompt(req.Protocol, req.Body)
}

func formatInt64(value int64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
