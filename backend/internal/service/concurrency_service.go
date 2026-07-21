package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"golang.org/x/sync/singleflight"
)

// ConcurrencyCache 定义并发控制的缓存接口
// 使用有序集合存储槽位，按时间戳清理过期条目
type ConcurrencyCache interface {
	// 账号槽位管理
	// 键格式: concurrency:account:{accountID}（有序集合，成员为 requestID）
	AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error)
	ReleaseAccountSlot(ctx context.Context, accountID int64, requestID string) error
	GetAccountConcurrency(ctx context.Context, accountID int64) (int, error)
	GetAccountConcurrencyBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error)

	// 账号等待队列（账号级）
	IncrementAccountWaitCount(ctx context.Context, accountID int64, maxWait int) (bool, error)
	DecrementAccountWaitCount(ctx context.Context, accountID int64) error
	GetAccountWaitingCount(ctx context.Context, accountID int64) (int, error)

	// 用户槽位管理
	// 键格式: concurrency:user:{userID}（有序集合，成员为 requestID）
	AcquireUserSlot(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error)
	ReleaseUserSlot(ctx context.Context, userID int64, requestID string) error
	GetUserConcurrency(ctx context.Context, userID int64) (int, error)

	// 等待队列计数（只在首次创建时设置 TTL）
	IncrementWaitCount(ctx context.Context, userID int64, maxWait int) (bool, error)
	DecrementWaitCount(ctx context.Context, userID int64) error

	// 批量负载查询（只读）
	GetAccountsLoadBatch(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error)
	GetUsersLoadBatch(ctx context.Context, users []UserWithConcurrency) (map[int64]*UserLoadInfo, error)

	// 清理过期槽位（后台任务）
	CleanupExpiredAccountSlots(ctx context.Context, accountID int64) error
	CleanupExpiredAccountSlotKeys(ctx context.Context) error

	// 启动时清理旧进程遗留槽位与等待计数
	CleanupStaleProcessSlots(ctx context.Context, activeRequestPrefix string) error
}

// ConcurrencySlotRefresher is optional so existing test and non-Redis caches
// keep working. Production Redis caches refresh the same member in-place.
type ConcurrencySlotRefresher interface {
	RefreshSlot(ctx context.Context, kind string, entityID int64, requestID string) error
}

type ConcurrencySlotTTLProvider interface {
	SlotTTL() time.Duration
}

type ConcurrencySlotRenewal struct {
	Kind      string
	EntityID  int64
	RequestID string
}

type ConcurrencySlotBatchRefresher interface {
	RefreshSlots(ctx context.Context, renewals []ConcurrencySlotRenewal) error
}

type APIKeyConcurrencyCache interface {
	TrackAPIKeySlot(ctx context.Context, apiKeyID int64, requestID string) error
	ReleaseAPIKeySlot(ctx context.Context, apiKeyID int64, requestID string) error
	GetAPIKeyConcurrencyBatch(ctx context.Context, apiKeyIDs []int64) (map[int64]int, error)
}

// AccountSlotCandidate is one account candidate for batched slot arbitration.
type AccountSlotCandidate struct {
	AccountID      int64
	MaxConcurrency int
	// Platform is the hard admission isolation domain. Empty preserves the
	// legacy single-Redis path for callers that do not have account metadata.
	Platform string
}

// PlatformAccountSlotCache is implemented by admission Cell aware caches.
// Keeping it optional preserves every existing cache implementation and test
// double when distributed admission is disabled.
type PlatformAccountSlotCache interface {
	AcquireAccountSlotForPlatform(ctx context.Context, platform string, accountID int64, maxConcurrency int, requestID string) (bool, error)
}

// AccountSlotArbitrationResult describes the account selected by Redis.
type AccountSlotArbitrationResult struct {
	Acquired    bool
	AccountID   int64
	RequestID   string
	ReleaseFunc func()
}

type UserAccountSlotArbitrationResult struct {
	Acquired         bool
	AccountID        int64
	UserRequestID    string
	AccountRequestID string
	ReleaseFunc      func()
	UserReleaseFunc  func()
}

// AccountSlotArbitrationCache is an optional fast-path implemented by Redis
// backed caches. It lets a scheduler submit a small ordered candidate window
// and atomically acquire the first available account slot in one round trip.
type AccountSlotArbitrationCache interface {
	AcquireFirstAvailableAccountSlot(ctx context.Context, candidates []AccountSlotCandidate, requestID string) (int64, bool, error)
}

type UserAccountSlotArbitrationCache interface {
	AcquireFirstAvailableUserAccountSlots(ctx context.Context, userID int64, userMaxConcurrency int, candidates []AccountSlotCandidate, userRequestID string, accountRequestID string) (int64, bool, error)
}

type AccountCooldownCache interface {
	SetAccountCooldown(ctx context.Context, accountID int64, ttl time.Duration) error
	ClearAccountCooldown(ctx context.Context, accountID int64) error
}

type VersionedAccountCooldownCache interface {
	SetAccountCooldownGeneration(ctx context.Context, accountID int64, ttl time.Duration, generation int64) error
	ClearAccountCooldownBeforeGeneration(ctx context.Context, accountID, generation int64) error
}

var (
	requestIDPrefix  = initRequestIDPrefix()
	requestIDCounter atomic.Uint64
)

func initRequestIDPrefix() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err == nil {
		return "r" + strconv.FormatUint(binary.BigEndian.Uint64(b), 36)
	}
	fallback := uint64(time.Now().UnixNano()) ^ (uint64(os.Getpid()) << 16)
	return "r" + strconv.FormatUint(fallback, 36)
}

func RequestIDPrefix() string {
	return requestIDPrefix
}

func generateRequestID() string {
	seq := requestIDCounter.Add(1)
	return requestIDPrefix + "-" + strconv.FormatUint(seq, 36)
}

func (s *ConcurrencyService) CleanupStaleProcessSlots(ctx context.Context) error {
	if s == nil || s.cache == nil {
		return nil
	}
	if s.staleProcessCleanupDone.Load() {
		return nil
	}
	s.staleProcessCleanupMu.Lock()
	defer s.staleProcessCleanupMu.Unlock()
	if s.staleProcessCleanupDone.Load() {
		return nil
	}
	if err := s.cache.CleanupStaleProcessSlots(ctx, RequestIDPrefix()); err != nil {
		return err
	}
	s.staleProcessCleanupDone.Store(true)
	return nil
}

// StartStaleProcessSlotCleanupRetry retries startup cleanup until Redis becomes
// ready. The periodic slot worker can complete the same latch sooner.
func (s *ConcurrencyService) StartStaleProcessSlotCleanupRetry() {
	if s == nil || s.cache == nil || !s.staleProcessRetryStarted.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.staleProcessRetryStarted.Store(false)
		backoff := time.Second
		for {
			time.Sleep(backoff)
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.CleanupStaleProcessSlots(cleanupCtx)
			cancel()
			if err == nil {
				logger.LegacyPrintf("service.concurrency", "Startup stale process slot cleanup recovered")
				return
			}
			logger.LegacyPrintf("service.concurrency", "Warning: retry cleanup stale process slots failed: %v", err)
			backoff = min(backoff*2, 30*time.Second)
		}
	}()
}

const (
	// 默认等待队列额外槽位
	defaultExtraWaitSlots = 20

	defaultAccountLoadBatchCacheTTL = 200 * time.Millisecond
	accountLoadBatchFetchTimeout    = 3 * time.Second
	maxAccountLoadBatchCacheEntries = 256
	apiKeyConcurrencyFetchTimeout   = 3 * time.Second
	apiKeySlotTrackTimeout          = 2 * time.Second
	slotReleaseMaxAttempts          = 3
	slotReleaseInitialTimeout       = 5 * time.Second
	slotReleaseRetryTimeout         = 2 * time.Second
	slotReleaseInitialBackoff       = 50 * time.Millisecond
	slotReleaseBackgroundBackoff    = 5 * time.Second
	slotReleaseBackgroundMaxBackoff = 30 * time.Second
	slotReleaseBackgroundMaxElapsed = 30 * time.Minute
	slotReleaseRetryWorkers         = 32
	slotReleaseRetryQueueSize       = 65536
)

type slotReleaseRetryJob struct {
	kind      string
	entityID  int64
	requestID string
	release   func(context.Context) error
	queuedAt  time.Time
}

var (
	slotReleaseRetryOnce  sync.Once
	slotReleaseRetryQueue chan slotReleaseRetryJob
)

func startSlotReleaseRetryWorkers() chan slotReleaseRetryJob {
	slotReleaseRetryOnce.Do(func() {
		slotReleaseRetryQueue = make(chan slotReleaseRetryJob, slotReleaseRetryQueueSize)
		for range slotReleaseRetryWorkers {
			go func() {
				for job := range slotReleaseRetryQueue {
					retryConcurrencySlotReleaseJob(job)
				}
			}()
		}
	})
	return slotReleaseRetryQueue
}

func retryConcurrencySlotReleaseInBackground(kind string, entityID int64, requestID string, release func(context.Context) error) {
	job := slotReleaseRetryJob{kind: kind, entityID: entityID, requestID: requestID, release: release, queuedAt: time.Now()}
	select {
	case startSlotReleaseRetryWorkers() <- job:
	default:
		logger.LegacyPrintf("service.concurrency", "Warning: release retry queue full for %s slot %d (req=%s); relying on slot TTL", kind, entityID, requestID)
	}
}

func retryConcurrencySlotReleaseJob(job slotReleaseRetryJob) {
	deadline := job.queuedAt.Add(slotReleaseBackgroundMaxElapsed)
	backoff := slotReleaseBackgroundBackoff
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(backoff)
		attemptCtx, cancel := context.WithTimeout(context.Background(), slotReleaseInitialTimeout)
		lastErr = job.release(attemptCtx)
		cancel()
		if lastErr == nil {
			logger.LegacyPrintf("service.concurrency", "Background release recovered for %s slot %d (req=%s)", job.kind, job.entityID, job.requestID)
			return
		}
		backoff = min(backoff*2, slotReleaseBackgroundMaxBackoff)
	}
	logger.LegacyPrintf("service.concurrency", "Warning: background release expired for %s slot %d (req=%s): %v", job.kind, job.entityID, job.requestID, lastErr)
}

func retryConcurrencySlotRelease(kind string, entityID int64, requestID string, release func(context.Context) error) {
	if release == nil {
		return
	}
	backoff := slotReleaseInitialBackoff
	var lastErr error
	for attempt := 1; attempt <= slotReleaseMaxAttempts; attempt++ {
		attemptTimeout := slotReleaseRetryTimeout
		if attempt == 1 {
			attemptTimeout = slotReleaseInitialTimeout
		}
		attemptCtx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		lastErr = release(attemptCtx)
		cancel()
		if lastErr == nil {
			return
		}
		if attempt < slotReleaseMaxAttempts {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	logger.LegacyPrintf("service.concurrency", "Warning: failed to release %s slot for %d (req=%s) after %d attempts, continuing in background: %v", kind, entityID, requestID, slotReleaseMaxAttempts, lastErr)
	retryConcurrencySlotReleaseInBackground(kind, entityID, requestID, release)
}

// ConcurrencyService 管理账号和用户的并发限制。
type ConcurrencyService struct {
	cache ConcurrencyCache

	accountLoadCacheTTL atomic.Int64
	accountLoadCacheMu  sync.RWMutex
	accountLoadCache    map[string]cachedAccountLoadBatch
	accountLoadGroup    singleflight.Group

	staleProcessCleanupMu    sync.Mutex
	staleProcessCleanupDone  atomic.Bool
	staleProcessRetryStarted atomic.Bool
	renewMu                  sync.Mutex
	renewals                 map[string]slotRenewal
	renewStop                chan struct{}
	renewDone                chan struct{}
	renewStopOnce            sync.Once
	renewInterval            time.Duration
}

type slotRenewal struct {
	kind      string
	entityID  int64
	requestID string
}

type cachedAccountLoadBatch struct {
	loadMap   map[int64]*AccountLoadInfo
	expiresAt time.Time
}

// NewConcurrencyService 创建并发控制服务。
func NewConcurrencyService(cache ConcurrencyCache) *ConcurrencyService {
	svc := &ConcurrencyService{
		cache:            cache,
		accountLoadCache: make(map[string]cachedAccountLoadBatch),
		renewals:         make(map[string]slotRenewal),
		renewStop:        make(chan struct{}),
		renewInterval:    5 * time.Minute,
	}
	svc.SetAccountLoadBatchCacheTTL(defaultAccountLoadBatchCacheTTL)
	if _, ok := cache.(ConcurrencySlotRefresher); ok {
		svc.renewDone = make(chan struct{})
		if provider, ok := cache.(ConcurrencySlotTTLProvider); ok {
			if ttl := provider.SlotTTL(); ttl > 0 {
				interval := ttl / 3
				if interval < 10*time.Second {
					interval = 10 * time.Second
				}
				if interval < svc.renewInterval {
					svc.renewInterval = interval
				}
			}
		}
		go svc.runSlotRenewalLoop()
	}
	return svc
}

func (s *ConcurrencyService) registerSlotRenewal(key, kind string, entityID int64, requestID string) {
	if s == nil || key == "" || kind == "" || entityID <= 0 || requestID == "" {
		return
	}
	s.renewMu.Lock()
	s.renewals[key] = slotRenewal{kind: kind, entityID: entityID, requestID: requestID}
	s.renewMu.Unlock()
}

func (s *ConcurrencyService) unregisterSlotRenewal(key string) {
	if s == nil || key == "" {
		return
	}
	s.renewMu.Lock()
	delete(s.renewals, key)
	s.renewMu.Unlock()
}

func (s *ConcurrencyService) runSlotRenewalLoop() {
	defer close(s.renewDone)
	ticker := time.NewTicker(s.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.renewMu.Lock()
			refreshers := make([]slotRenewal, 0, len(s.renewals))
			for _, renewal := range s.renewals {
				refreshers = append(refreshers, renewal)
			}
			s.renewMu.Unlock()
			if batch, ok := s.cache.(ConcurrencySlotBatchRefresher); ok {
				renewals := make([]ConcurrencySlotRenewal, len(refreshers))
				for i, renewal := range refreshers {
					renewals[i] = ConcurrencySlotRenewal{
						Kind: renewal.kind, EntityID: renewal.entityID, RequestID: renewal.requestID,
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				_ = batch.RefreshSlots(ctx, renewals)
				cancel()
				continue
			}
			jobs := make(chan slotRenewal, 256)
			var workers sync.WaitGroup
			for i := 0; i < 32; i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for renewal := range jobs {
						ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
						if refresher, ok := s.cache.(ConcurrencySlotRefresher); ok {
							_ = refresher.RefreshSlot(ctx, renewal.kind, renewal.entityID, renewal.requestID)
						}
						cancel()
					}
				}()
			}
			for _, refresh := range refreshers {
				jobs <- refresh
			}
			close(jobs)
			workers.Wait()
		case <-s.renewStop:
			return
		}
	}
}

func (s *ConcurrencyService) StopBackgroundWorkers() {
	if s == nil {
		return
	}
	s.renewStopOnce.Do(func() { close(s.renewStop) })
	if s.renewDone != nil {
		<-s.renewDone
	}
	if closer, ok := s.cache.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			logger.LegacyPrintf("service.concurrency", "Warning: close admission cache: %v", err)
		}
	}
}

// SetAccountLoadBatchCacheTTL 设置账号负载批量读取的极短 TTL 缓存；非正数表示禁用缓存。
func (s *ConcurrencyService) SetAccountLoadBatchCacheTTL(ttl time.Duration) {
	if s == nil {
		return
	}
	s.accountLoadCacheTTL.Store(int64(ttl))
	if ttl <= 0 {
		s.accountLoadCacheMu.Lock()
		s.accountLoadCache = make(map[string]cachedAccountLoadBatch)
		s.accountLoadCacheMu.Unlock()
	}
}

// AcquireResult represents the result of acquiring a concurrency slot
type AcquireResult struct {
	Acquired    bool
	ReleaseFunc func() // Must be called when done (typically via defer)
}

type AccountWithConcurrency struct {
	ID             int64
	MaxConcurrency int
}

type UserWithConcurrency struct {
	ID             int64
	MaxConcurrency int
}

type AccountLoadInfo struct {
	AccountID          int64
	CurrentConcurrency int
	WaitingCount       int
	LoadRate           int // 0-100+ (percent)
}

type UserLoadInfo struct {
	UserID             int64
	CurrentConcurrency int
	WaitingCount       int
	LoadRate           int // 0-100+ (percent)
}

// AcquireAccountSlot attempts to acquire a concurrency slot for an account.
// If the account is at max concurrency, it waits until a slot is available or timeout.
// Returns a release function that MUST be called when the request completes.
func (s *ConcurrencyService) AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int) (*AcquireResult, error) {
	return s.AcquireAccountSlotForPlatform(ctx, "", accountID, maxConcurrency)
}

func (s *ConcurrencyService) AcquireAccountSlotForPlatform(ctx context.Context, platform string, accountID int64, maxConcurrency int) (*AcquireResult, error) {
	// If maxConcurrency is 0 or negative, no limit
	if maxConcurrency <= 0 {
		return &AcquireResult{
			Acquired:    true,
			ReleaseFunc: func() {}, // no-op
		}, nil
	}

	// Generate unique request ID for this slot
	requestID := generateRequestID()

	var acquired bool
	var err error
	if platformCache, ok := s.cache.(PlatformAccountSlotCache); ok && platform != "" {
		acquired, err = platformCache.AcquireAccountSlotForPlatform(ctx, platform, accountID, maxConcurrency, requestID)
	} else {
		acquired, err = s.cache.AcquireAccountSlot(ctx, accountID, maxConcurrency, requestID)
	}
	if err != nil {
		return nil, err
	}

	if acquired {
		renewalKey := "account:" + strconv.FormatInt(accountID, 10) + ":" + requestID
		if _, ok := s.cache.(ConcurrencySlotRefresher); ok {
			s.registerSlotRenewal(renewalKey, "account", accountID, requestID)
		}
		return &AcquireResult{
			Acquired: true,
			ReleaseFunc: func() {
				s.unregisterSlotRenewal(renewalKey)
				retryConcurrencySlotRelease("account", accountID, requestID, func(ctx context.Context) error {
					return s.cache.ReleaseAccountSlot(ctx, accountID, requestID)
				})
			},
		}, nil
	}

	return &AcquireResult{
		Acquired:    false,
		ReleaseFunc: nil,
	}, nil
}

func (s *ConcurrencyService) AcquireFirstAvailableAccountSlot(ctx context.Context, candidates []AccountSlotCandidate) (*AccountSlotArbitrationResult, error) {
	if len(candidates) == 0 {
		return &AccountSlotArbitrationResult{}, nil
	}
	for _, candidate := range candidates {
		if candidate.AccountID <= 0 {
			continue
		}
		if candidate.MaxConcurrency <= 0 {
			return &AccountSlotArbitrationResult{
				Acquired:    true,
				AccountID:   candidate.AccountID,
				ReleaseFunc: func() {},
			}, nil
		}
		break
	}
	if s == nil || s.cache == nil {
		for _, candidate := range candidates {
			if candidate.AccountID > 0 {
				return &AccountSlotArbitrationResult{
					Acquired:    true,
					AccountID:   candidate.AccountID,
					ReleaseFunc: func() {},
				}, nil
			}
		}
		return &AccountSlotArbitrationResult{}, nil
	}
	fastCache, ok := s.cache.(AccountSlotArbitrationCache)
	if !ok {
		return nil, nil
	}

	requestID := generateRequestID()
	accountID, acquired, err := fastCache.AcquireFirstAvailableAccountSlot(ctx, candidates, requestID)
	if err != nil {
		return nil, err
	}
	if !acquired || accountID <= 0 {
		return &AccountSlotArbitrationResult{Acquired: false}, nil
	}
	renewalKey := "account:" + strconv.FormatInt(accountID, 10) + ":" + requestID
	if _, ok := s.cache.(ConcurrencySlotRefresher); ok {
		s.registerSlotRenewal(renewalKey, "account", accountID, requestID)
	}
	return &AccountSlotArbitrationResult{
		Acquired:  true,
		AccountID: accountID,
		RequestID: requestID,
		ReleaseFunc: func() {
			s.unregisterSlotRenewal(renewalKey)
			retryConcurrencySlotRelease("arbitrated account", accountID, requestID, func(ctx context.Context) error {
				return s.cache.ReleaseAccountSlot(ctx, accountID, requestID)
			})
		},
	}, nil
}

func (s *ConcurrencyService) AcquireFirstAvailableUserAccountSlots(
	ctx context.Context,
	userID int64,
	userMaxConcurrency int,
	candidates []AccountSlotCandidate,
) (*UserAccountSlotArbitrationResult, error) {
	if len(candidates) == 0 {
		return &UserAccountSlotArbitrationResult{}, nil
	}
	if userMaxConcurrency <= 0 {
		accountResult, err := s.AcquireFirstAvailableAccountSlot(ctx, candidates)
		if err != nil || accountResult == nil {
			return nil, err
		}
		return &UserAccountSlotArbitrationResult{
			Acquired:         accountResult.Acquired,
			AccountID:        accountResult.AccountID,
			AccountRequestID: accountResult.RequestID,
			ReleaseFunc:      accountResult.ReleaseFunc,
			UserReleaseFunc:  func() {},
		}, nil
	}
	if s == nil || s.cache == nil {
		accountResult, err := s.AcquireFirstAvailableAccountSlot(ctx, candidates)
		if err != nil || accountResult == nil {
			return nil, err
		}
		return &UserAccountSlotArbitrationResult{
			Acquired:         accountResult.Acquired,
			AccountID:        accountResult.AccountID,
			AccountRequestID: accountResult.RequestID,
			ReleaseFunc:      accountResult.ReleaseFunc,
			UserReleaseFunc:  func() {},
		}, nil
	}
	fastCache, ok := s.cache.(UserAccountSlotArbitrationCache)
	if !ok {
		return nil, nil
	}

	userRequestID := generateRequestID()
	accountRequestID := generateRequestID()
	accountID, acquired, err := fastCache.AcquireFirstAvailableUserAccountSlots(ctx, userID, userMaxConcurrency, candidates, userRequestID, accountRequestID)
	if err != nil {
		return nil, err
	}
	if !acquired || accountID <= 0 {
		return &UserAccountSlotArbitrationResult{Acquired: false}, nil
	}
	userRenewalKey := "user:" + strconv.FormatInt(userID, 10) + ":" + userRequestID
	accountRenewalKey := "account:" + strconv.FormatInt(accountID, 10) + ":" + accountRequestID
	if _, ok := s.cache.(ConcurrencySlotRefresher); ok {
		s.registerSlotRenewal(userRenewalKey, "user", userID, userRequestID)
		s.registerSlotRenewal(accountRenewalKey, "account", accountID, accountRequestID)
	}
	releaseUser := func() {
		s.unregisterSlotRenewal(userRenewalKey)
		retryConcurrencySlotRelease("arbitrated user", userID, userRequestID, func(ctx context.Context) error {
			return s.cache.ReleaseUserSlot(ctx, userID, userRequestID)
		})
	}
	releaseAccount := func() {
		s.unregisterSlotRenewal(accountRenewalKey)
		retryConcurrencySlotRelease("arbitrated account", accountID, accountRequestID, func(ctx context.Context) error {
			return s.cache.ReleaseAccountSlot(ctx, accountID, accountRequestID)
		})
	}
	return &UserAccountSlotArbitrationResult{
		Acquired:         true,
		AccountID:        accountID,
		UserRequestID:    userRequestID,
		AccountRequestID: accountRequestID,
		ReleaseFunc:      releaseAccount,
		UserReleaseFunc:  releaseUser,
	}, nil
}

func (s *ConcurrencyService) SetAccountCooldown(ctx context.Context, accountID int64, until time.Time) error {
	if s == nil || s.cache == nil || accountID <= 0 || until.IsZero() {
		return nil
	}
	ttl := time.Until(until)
	if ttl <= 0 {
		return s.ClearAccountCooldown(ctx, accountID)
	}
	cooldownCache, ok := s.cache.(AccountCooldownCache)
	if !ok {
		return nil
	}
	return cooldownCache.SetAccountCooldown(ctx, accountID, ttl)
}

func (s *ConcurrencyService) ClearAccountCooldown(ctx context.Context, accountID int64) error {
	if s == nil || s.cache == nil || accountID <= 0 {
		return nil
	}
	cooldownCache, ok := s.cache.(AccountCooldownCache)
	if !ok {
		return nil
	}
	return cooldownCache.ClearAccountCooldown(ctx, accountID)
}

func (s *ConcurrencyService) SetAccountCooldownGeneration(ctx context.Context, accountID int64, until time.Time, generation int64) error {
	if s == nil || s.cache == nil || accountID <= 0 || generation < 0 {
		return nil
	}
	ttl := time.Until(until)
	if ttl <= 0 {
		return s.ClearAccountCooldownBeforeGeneration(ctx, accountID, generation+1)
	}
	cache, ok := s.cache.(VersionedAccountCooldownCache)
	if !ok {
		return s.SetAccountCooldown(ctx, accountID, until)
	}
	return cache.SetAccountCooldownGeneration(ctx, accountID, ttl, generation)
}

func (s *ConcurrencyService) ClearAccountCooldownBeforeGeneration(ctx context.Context, accountID, generation int64) error {
	if s == nil || s.cache == nil || accountID <= 0 || generation <= 0 {
		return nil
	}
	cache, ok := s.cache.(VersionedAccountCooldownCache)
	if !ok {
		return s.ClearAccountCooldown(ctx, accountID)
	}
	return cache.ClearAccountCooldownBeforeGeneration(ctx, accountID, generation)
}

// AcquireUserSlot attempts to acquire a concurrency slot for a user.
// If the user is at max concurrency, it waits until a slot is available or timeout.
// Returns a release function that MUST be called when the request completes.
func (s *ConcurrencyService) AcquireUserSlot(ctx context.Context, userID int64, maxConcurrency int) (*AcquireResult, error) {
	// If maxConcurrency is 0 or negative, no limit
	if maxConcurrency <= 0 {
		return &AcquireResult{
			Acquired:    true,
			ReleaseFunc: func() {}, // no-op
		}, nil
	}

	// Generate unique request ID for this slot
	requestID := generateRequestID()

	acquired, err := s.cache.AcquireUserSlot(ctx, userID, maxConcurrency, requestID)
	if err != nil {
		return nil, err
	}

	if acquired {
		renewalKey := "user:" + strconv.FormatInt(userID, 10) + ":" + requestID
		if _, ok := s.cache.(ConcurrencySlotRefresher); ok {
			s.registerSlotRenewal(renewalKey, "user", userID, requestID)
		}
		return &AcquireResult{
			Acquired: true,
			ReleaseFunc: func() {
				s.unregisterSlotRenewal(renewalKey)
				retryConcurrencySlotRelease("user", userID, requestID, func(ctx context.Context) error {
					return s.cache.ReleaseUserSlot(ctx, userID, requestID)
				})
			},
		}, nil
	}

	return &AcquireResult{
		Acquired:    false,
		ReleaseFunc: nil,
	}, nil
}

// TrackAPIKeySlot records one active request slot for an API key without
// applying key-level concurrency limits. It is best-effort: Redis errors are
// logged and return a no-op release function so request forwarding is not blocked.
func (s *ConcurrencyService) TrackAPIKeySlot(ctx context.Context, apiKeyID int64) func() {
	if s == nil || s.cache == nil || apiKeyID <= 0 {
		return func() {}
	}
	cache, ok := s.cache.(APIKeyConcurrencyCache)
	if !ok {
		return func() {}
	}

	requestID := generateRequestID()
	baseCtx := context.Background()
	if ctx != nil {
		baseCtx = context.WithoutCancel(ctx)
	}
	trackCtx, cancel := context.WithTimeout(baseCtx, apiKeySlotTrackTimeout)
	err := cache.TrackAPIKeySlot(trackCtx, apiKeyID, requestID)
	cancel()
	if err != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: failed to track api key slot for %d (req=%s): %v", apiKeyID, requestID, err)
		return func() {}
	}
	renewalKey := "api_key:" + strconv.FormatInt(apiKeyID, 10) + ":" + requestID
	if _, ok := s.cache.(ConcurrencySlotRefresher); ok {
		s.registerSlotRenewal(renewalKey, "api_key", apiKeyID, requestID)
	}

	return func() {
		s.unregisterSlotRenewal(renewalKey)
		retryConcurrencySlotRelease("api key", apiKeyID, requestID, func(ctx context.Context) error {
			return cache.ReleaseAPIKeySlot(ctx, apiKeyID, requestID)
		})
	}
}

// GetAPIKeyConcurrencyBatch gets real-time active request counts for API keys.
// Stats are best-effort: missing Redis support or Redis errors return zeroes.
func (s *ConcurrencyService) GetAPIKeyConcurrencyBatch(ctx context.Context, apiKeyIDs []int64) (map[int64]int, error) {
	result := zeroAPIKeyConcurrencyMap(apiKeyIDs)
	if len(apiKeyIDs) == 0 {
		return result, nil
	}
	if s == nil || s.cache == nil {
		return result, nil
	}
	cache, ok := s.cache.(APIKeyConcurrencyCache)
	if !ok {
		return result, nil
	}

	redisCtx, cancel := context.WithTimeout(context.Background(), apiKeyConcurrencyFetchTimeout)
	defer cancel()

	counts, err := cache.GetAPIKeyConcurrencyBatch(redisCtx, apiKeyIDs)
	if err != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: get api key concurrency batch failed: %v", err)
		return result, nil
	}
	for _, apiKeyID := range apiKeyIDs {
		result[apiKeyID] = counts[apiKeyID]
	}
	return result, nil
}

func zeroAPIKeyConcurrencyMap(apiKeyIDs []int64) map[int64]int {
	result := make(map[int64]int, len(apiKeyIDs))
	for _, apiKeyID := range apiKeyIDs {
		result[apiKeyID] = 0
	}
	return result
}

// ============================================
// Wait Queue Count Methods
// ============================================

// IncrementWaitCount attempts to increment the wait queue counter for a user.
// Returns true if successful, false if the wait queue is full.
// maxWait should be user.Concurrency + defaultExtraWaitSlots
func (s *ConcurrencyService) IncrementWaitCount(ctx context.Context, userID int64, maxWait int) (bool, error) {
	if s.cache == nil {
		// Redis not available, allow request
		return true, nil
	}

	result, err := s.cache.IncrementWaitCount(ctx, userID, maxWait)
	if err != nil {
		// On error, allow the request to proceed (fail open)
		logger.LegacyPrintf("service.concurrency", "Warning: increment wait count failed for user %d: %v", userID, err)
		return true, nil
	}
	return result, nil
}

// DecrementWaitCount decrements the wait queue counter for a user.
// Should be called when a request completes or exits the wait queue.
func (s *ConcurrencyService) DecrementWaitCount(ctx context.Context, userID int64) {
	if s.cache == nil {
		return
	}

	// Use background context to ensure decrement even if original context is cancelled
	bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.cache.DecrementWaitCount(bgCtx, userID); err != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: decrement wait count failed for user %d: %v", userID, err)
	}
}

// IncrementAccountWaitCount increments the wait queue counter for an account.
func (s *ConcurrencyService) IncrementAccountWaitCount(ctx context.Context, accountID int64, maxWait int) (bool, error) {
	if s.cache == nil {
		return true, nil
	}

	result, err := s.cache.IncrementAccountWaitCount(ctx, accountID, maxWait)
	if err != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: increment wait count failed for account %d: %v", accountID, err)
		return true, nil
	}
	return result, nil
}

// DecrementAccountWaitCount decrements the wait queue counter for an account.
func (s *ConcurrencyService) DecrementAccountWaitCount(ctx context.Context, accountID int64) {
	if s.cache == nil {
		return
	}

	bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.cache.DecrementAccountWaitCount(bgCtx, accountID); err != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: decrement wait count failed for account %d: %v", accountID, err)
	}
}

// GetAccountWaitingCount gets current wait queue count for an account.
func (s *ConcurrencyService) GetAccountWaitingCount(ctx context.Context, accountID int64) (int, error) {
	if s.cache == nil {
		return 0, nil
	}
	return s.cache.GetAccountWaitingCount(ctx, accountID)
}

// CalculateMaxWait calculates the maximum wait queue size for a user
// maxWait = userConcurrency + defaultExtraWaitSlots
func CalculateMaxWait(userConcurrency int) int {
	if userConcurrency <= 0 {
		userConcurrency = 1
	}
	return userConcurrency + defaultExtraWaitSlots
}

// GetAccountsLoadBatch 批量获取账号负载信息。
func (s *ConcurrencyService) GetAccountsLoadBatch(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	return s.getAccountsLoadBatch(ctx, accounts, true)
}

// GetAccountsLoadBatchFresh 绕过极短 TTL 缓存，用于抢槽失败后的实时刷新兜底。
func (s *ConcurrencyService) GetAccountsLoadBatchFresh(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	return s.getAccountsLoadBatch(ctx, accounts, false)
}

func (s *ConcurrencyService) getAccountsLoadBatch(ctx context.Context, accounts []AccountWithConcurrency, allowCache bool) (map[int64]*AccountLoadInfo, error) {
	if len(accounts) == 0 {
		return map[int64]*AccountLoadInfo{}, nil
	}
	if s.cache == nil {
		return map[int64]*AccountLoadInfo{}, nil
	}

	ttl := time.Duration(s.accountLoadCacheTTL.Load())
	if !allowCache || ttl <= 0 {
		return s.fetchAccountsLoadBatch(ctx, accounts)
	}

	key := accountLoadBatchCacheKey(accounts)
	if cached, ok := s.getCachedAccountLoadBatch(key, time.Now()); ok {
		return cached, nil
	}

	value, err, _ := s.accountLoadGroup.Do(key, func() (any, error) {
		now := time.Now()
		if cached, ok := s.getCachedAccountLoadBatch(key, now); ok {
			return cached, nil
		}
		loadMap, fetchErr := s.fetchAccountsLoadBatch(ctx, accounts)
		if fetchErr != nil {
			return nil, fetchErr
		}
		cached := cloneAccountLoadMap(loadMap)
		s.storeCachedAccountLoadBatch(key, cached, now.Add(ttl))
		return cached, nil
	})
	if err != nil {
		return nil, err
	}
	loadMap, _ := value.(map[int64]*AccountLoadInfo)
	if loadMap == nil {
		return map[int64]*AccountLoadInfo{}, nil
	}
	return loadMap, nil
}

func (s *ConcurrencyService) fetchAccountsLoadBatch(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	if s.cache == nil {
		return map[int64]*AccountLoadInfo{}, nil
	}
	baseCtx := context.Background()
	if ctx != nil {
		baseCtx = context.WithoutCancel(ctx)
	}
	redisCtx, cancel := context.WithTimeout(baseCtx, accountLoadBatchFetchTimeout)
	defer cancel()
	return s.cache.GetAccountsLoadBatch(redisCtx, accounts)
}

func (s *ConcurrencyService) getCachedAccountLoadBatch(key string, now time.Time) (map[int64]*AccountLoadInfo, bool) {
	s.accountLoadCacheMu.RLock()
	cached, ok := s.accountLoadCache[key]
	s.accountLoadCacheMu.RUnlock()
	if !ok {
		return nil, false
	}
	if !now.Before(cached.expiresAt) {
		s.accountLoadCacheMu.Lock()
		if current, exists := s.accountLoadCache[key]; exists && !now.Before(current.expiresAt) {
			delete(s.accountLoadCache, key)
		}
		s.accountLoadCacheMu.Unlock()
		return nil, false
	}
	return cached.loadMap, true
}

func (s *ConcurrencyService) storeCachedAccountLoadBatch(key string, loadMap map[int64]*AccountLoadInfo, expiresAt time.Time) {
	s.accountLoadCacheMu.Lock()
	if s.accountLoadCache == nil {
		s.accountLoadCache = make(map[string]cachedAccountLoadBatch)
	}
	if len(s.accountLoadCache) >= maxAccountLoadBatchCacheEntries {
		now := time.Now()
		for cacheKey, cached := range s.accountLoadCache {
			if !now.Before(cached.expiresAt) {
				delete(s.accountLoadCache, cacheKey)
			}
		}
		for len(s.accountLoadCache) >= maxAccountLoadBatchCacheEntries {
			for cacheKey := range s.accountLoadCache {
				delete(s.accountLoadCache, cacheKey)
				break
			}
		}
	}
	s.accountLoadCache[key] = cachedAccountLoadBatch{
		loadMap:   loadMap,
		expiresAt: expiresAt,
	}
	s.accountLoadCacheMu.Unlock()
}

func accountLoadBatchCacheKey(accounts []AccountWithConcurrency) string {
	hash := sha256.New()
	var buf [16]byte
	for _, account := range accounts {
		binary.LittleEndian.PutUint64(buf[:8], uint64(account.ID))
		binary.LittleEndian.PutUint64(buf[8:], uint64(int64(account.MaxConcurrency)))
		_, _ = hash.Write(buf[:])
	}
	sum := hash.Sum(nil)
	return strconv.Itoa(len(accounts)) + ":" + hex.EncodeToString(sum)
}

func cloneAccountLoadMap(loadMap map[int64]*AccountLoadInfo) map[int64]*AccountLoadInfo {
	if len(loadMap) == 0 {
		return map[int64]*AccountLoadInfo{}
	}
	clone := make(map[int64]*AccountLoadInfo, len(loadMap))
	for accountID, loadInfo := range loadMap {
		if loadInfo == nil {
			clone[accountID] = nil
			continue
		}
		copied := *loadInfo
		clone[accountID] = &copied
	}
	return clone
}

// GetUsersLoadBatch returns load info for multiple users.
func (s *ConcurrencyService) GetUsersLoadBatch(ctx context.Context, users []UserWithConcurrency) (map[int64]*UserLoadInfo, error) {
	if s.cache == nil {
		return map[int64]*UserLoadInfo{}, nil
	}
	return s.cache.GetUsersLoadBatch(ctx, users)
}

// CleanupExpiredAccountSlots removes expired slots for one account (background task).
func (s *ConcurrencyService) CleanupExpiredAccountSlots(ctx context.Context, accountID int64) error {
	if s.cache == nil {
		return nil
	}
	return s.cache.CleanupExpiredAccountSlots(ctx, accountID)
}

// StartSlotCleanupWorker starts a background cleanup worker for expired account slots.
func (s *ConcurrencyService) StartSlotCleanupWorker(_ AccountRepository, interval time.Duration) {
	if s == nil || s.cache == nil || interval <= 0 {
		return
	}

	runCleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.cache.CleanupExpiredAccountSlotKeys(cleanupCtx)
		cancel()
		if err != nil {
			logger.LegacyPrintf("service.concurrency", "Warning: cleanup expired account slots failed: %v", err)
			return
		}
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		runCleanup()
		for {
			select {
			case <-ticker.C:
				runCleanup()
			case <-s.renewStop:
				return
			}
		}
	}()
}

// GetAccountConcurrencyBatch gets current concurrency counts for multiple accounts.
// Uses a detached context with timeout to prevent HTTP request cancellation from
// causing the entire batch to fail (which would show all concurrency as 0).
func (s *ConcurrencyService) GetAccountConcurrencyBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error) {
	if len(accountIDs) == 0 {
		return map[int64]int{}, nil
	}
	if s.cache == nil {
		result := make(map[int64]int, len(accountIDs))
		for _, accountID := range accountIDs {
			result[accountID] = 0
		}
		return result, nil
	}

	// Use a detached context so that a cancelled HTTP request doesn't cause
	// the Redis pipeline to fail and return all-zero concurrency counts.
	redisCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	return s.cache.GetAccountConcurrencyBatch(redisCtx, accountIDs)
}
