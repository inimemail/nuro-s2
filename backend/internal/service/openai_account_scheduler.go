package service

import (
	"container/heap"
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	openAIAccountScheduleLayerPreviousResponse = "previous_response_id"
	openAIAccountScheduleLayerSessionSticky    = "session_hash"
	openAIAccountScheduleLayerLoadBalance      = "load_balance"
	openAIAdvancedSchedulerSettingKey          = "openai_advanced_scheduler_enabled"
)

const (
	openAIAdvancedSchedulerSettingCacheTTL  = 5 * time.Second
	openAIAdvancedSchedulerSettingDBTimeout = 2 * time.Second
)

const (
	openAIQuotaHeadroomNeutralFactor      = 0.5
	openAIQuotaHeadroomSecondaryLowRemain = 0.10
	openAIQuotaHeadroomSnapshotStaleAfter = 8 * time.Hour
)

type cachedOpenAIAdvancedSchedulerSetting struct {
	enabled   bool
	expiresAt int64
}

var openAIAdvancedSchedulerSettingCache atomic.Value // *cachedOpenAIAdvancedSchedulerSetting
var openAIAdvancedSchedulerSettingSF singleflight.Group

type OpenAIAccountScheduleRequest struct {
	GroupID                 *int64
	UserID                  int64
	UserConcurrency         int
	SessionHash             string
	StickyAccountID         int64
	PreviousResponseID      string
	RequestedModel          string
	RequiredTransport       OpenAIUpstreamTransport
	RequiredCapability      OpenAIEndpointCapability
	RequiredImageCapability OpenAIImagesCapability
	RequireCompact          bool
	RequestPlatform         string
	ExcludedIDs             map[int64]struct{}
	LockedPriority          int
	PreserveStickyBinding   bool
}

type OpenAIAccountScheduleDecision struct {
	Layer               string
	StickyPreviousHit   bool
	StickySessionHit    bool
	CandidateCount      int
	TopK                int
	LatencyMs           int64
	LoadSkew            float64
	SelectedAccountID   int64
	SelectedAccountType string
}

type OpenAIAccountSchedulerMetricsSnapshot struct {
	SelectTotal              int64
	StickyPreviousHitTotal   int64
	StickySessionHitTotal    int64
	LoadBalanceSelectTotal   int64
	AccountSwitchTotal       int64
	SchedulerLatencyMsTotal  int64
	SchedulerLatencyMsAvg    float64
	StickyHitRatio           float64
	AccountSwitchRate        float64
	LoadSkewAvg              float64
	RuntimeStatsAccountCount int
}

type OpenAIAccountScheduler interface {
	Select(ctx context.Context, req OpenAIAccountScheduleRequest) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error)
	ReportResult(accountID int64, success bool, firstTokenMs *int)
	ReportResultForRequest(accountID int64, success bool, firstTokenMs *int, requiredImageCapability OpenAIImagesCapability)
	ReportResultForRoute(accountID int64, success bool, firstTokenMs *int, requestedModel string, transport OpenAIUpstreamTransport)
	ReportSwitch()
	SnapshotMetrics() OpenAIAccountSchedulerMetricsSnapshot
}

type openAIAccountSchedulerMetrics struct {
	selectTotal            atomic.Int64
	stickyPreviousHitTotal atomic.Int64
	stickySessionHitTotal  atomic.Int64
	loadBalanceSelectTotal atomic.Int64
	accountSwitchTotal     atomic.Int64
	latencyMsTotal         atomic.Int64
	loadSkewMilliTotal     atomic.Int64
}

type openAIAccountLoadPlan struct {
	allCandidates             []openAIAccountCandidateScore
	candidates                []openAIAccountCandidateScore
	staleSnapshotCompactRetry []openAIAccountCandidateScore
	selectionOrder            []openAIAccountCandidateScore
	candidateCount            int
	topK                      int
	loadSkew                  float64
}

func (m *openAIAccountSchedulerMetrics) recordSelect(decision OpenAIAccountScheduleDecision) {
	if m == nil {
		return
	}
	m.selectTotal.Add(1)
	m.latencyMsTotal.Add(decision.LatencyMs)
	m.loadSkewMilliTotal.Add(int64(math.Round(decision.LoadSkew * 1000)))
	if decision.StickyPreviousHit {
		m.stickyPreviousHitTotal.Add(1)
	}
	if decision.StickySessionHit {
		m.stickySessionHitTotal.Add(1)
	}
	if decision.Layer == openAIAccountScheduleLayerLoadBalance {
		m.loadBalanceSelectTotal.Add(1)
	}
}

func (m *openAIAccountSchedulerMetrics) recordSwitch() {
	if m == nil {
		return
	}
	m.accountSwitchTotal.Add(1)
}

type openAIAccountRuntimeStats struct {
	accounts           sync.Map
	accountIDs         sync.Map
	accountCount       atomic.Int64
	selectionCounter   atomic.Uint64
	unknownExploreAt   sync.Map
	degradedRecoveryAt sync.Map
}

type openAIAccountRuntimeStatsKey struct {
	accountID int64
	kind      string
	model     string
	transport string
}

type openAIAccountRuntimeStat struct {
	errorRateEWMABits atomic.Uint64
	ttftEWMABits      atomic.Uint64
	sampleCount       atomic.Int64
	ttftSampleCount   atomic.Int64
	lastUpdatedNano   atomic.Int64
}

func newOpenAIAccountRuntimeStats() *openAIAccountRuntimeStats {
	return &openAIAccountRuntimeStats{}
}

func (s *openAIAccountRuntimeStats) loadOrCreate(accountID int64) *openAIAccountRuntimeStat {
	return s.loadOrCreateForKey(openAIAccountRuntimeStatsKey{accountID: accountID, kind: "text"})
}

func (s *openAIAccountRuntimeStats) loadOrCreateForKey(key openAIAccountRuntimeStatsKey) *openAIAccountRuntimeStat {
	if key.accountID <= 0 {
		return nil
	}
	key = normalizeOpenAIAccountRuntimeStatsKey(key)
	if value, ok := s.accounts.Load(key); ok {
		stat, _ := value.(*openAIAccountRuntimeStat)
		if stat != nil {
			return stat
		}
	}

	stat := &openAIAccountRuntimeStat{}
	stat.ttftEWMABits.Store(math.Float64bits(math.NaN()))
	actual, loaded := s.accounts.LoadOrStore(key, stat)
	if !loaded {
		if _, accountLoaded := s.accountIDs.LoadOrStore(key.accountID, struct{}{}); !accountLoaded {
			s.accountCount.Add(1)
		}
		return stat
	}
	existing, _ := actual.(*openAIAccountRuntimeStat)
	if existing != nil {
		return existing
	}
	return stat
}

func updateEWMAAtomic(target *atomic.Uint64, sample float64, alpha float64) {
	for {
		oldBits := target.Load()
		oldValue := math.Float64frombits(oldBits)
		newValue := alpha*sample + (1-alpha)*oldValue
		if target.CompareAndSwap(oldBits, math.Float64bits(newValue)) {
			return
		}
	}
}

func (s *openAIAccountRuntimeStats) report(accountID int64, success bool, firstTokenMs *int) {
	s.reportForRequest(accountID, success, firstTokenMs, "")
}

func openAIAccountRuntimeStatsKind(requiredImageCapability OpenAIImagesCapability) string {
	if requiredImageCapability != "" {
		return "images"
	}
	return "text"
}

func (s *openAIAccountRuntimeStats) reportForRequest(accountID int64, success bool, firstTokenMs *int, requiredImageCapability OpenAIImagesCapability) {
	s.reportForKey(openAIAccountRuntimeStatsKey{
		accountID: accountID,
		kind:      openAIAccountRuntimeStatsKind(requiredImageCapability),
	}, success, firstTokenMs)
}

func (s *openAIAccountRuntimeStats) reportForRoute(accountID int64, success bool, firstTokenMs *int, requestedModel string, transport OpenAIUpstreamTransport) {
	s.reportForKey(openAIAccountRuntimeStatsKey{
		accountID: accountID,
		kind:      "text",
		model:     requestedModel,
		transport: string(transport),
	}, success, firstTokenMs)
}

func (s *openAIAccountRuntimeStats) reportForKey(key openAIAccountRuntimeStatsKey, success bool, firstTokenMs *int) {
	if s == nil || key.accountID <= 0 {
		return
	}
	const alpha = 0.2
	stat := s.loadOrCreateForKey(key)
	if stat == nil {
		return
	}
	stat.sampleCount.Add(1)
	stat.lastUpdatedNano.Store(time.Now().UnixNano())

	errorSample := 1.0
	if success {
		errorSample = 0.0
	}
	updateEWMAAtomic(&stat.errorRateEWMABits, errorSample, alpha)

	if firstTokenMs != nil && *firstTokenMs > 0 {
		stat.ttftSampleCount.Add(1)
		ttft := float64(*firstTokenMs)
		ttftBits := math.Float64bits(ttft)
		for {
			oldBits := stat.ttftEWMABits.Load()
			oldValue := math.Float64frombits(oldBits)
			if math.IsNaN(oldValue) {
				if stat.ttftEWMABits.CompareAndSwap(oldBits, ttftBits) {
					break
				}
				continue
			}
			newValue := alpha*ttft + (1-alpha)*oldValue
			if stat.ttftEWMABits.CompareAndSwap(oldBits, math.Float64bits(newValue)) {
				break
			}
		}
	}
}

func (s *openAIAccountRuntimeStats) snapshot(accountID int64) (errorRate float64, ttft float64, hasTTFT bool) {
	return s.snapshotForRequest(accountID, "")
}

func (s *openAIAccountRuntimeStats) snapshotForRequest(accountID int64, requiredImageCapability OpenAIImagesCapability) (errorRate float64, ttft float64, hasTTFT bool) {
	errorRate, ttft, hasTTFT, _ = s.snapshotForKey(openAIAccountRuntimeStatsKey{
		accountID: accountID,
		kind:      openAIAccountRuntimeStatsKind(requiredImageCapability),
	})
	return errorRate, ttft, hasTTFT
}

func (s *openAIAccountRuntimeStats) snapshotForRequestWithMeta(accountID int64, requiredImageCapability OpenAIImagesCapability) (errorRate float64, ttft float64, hasTTFT bool, sampleCount int64, ttftSampleCount int64, lastUpdated time.Time) {
	errorRate, ttft, hasTTFT, _, sampleCount, ttftSampleCount, lastUpdated = s.snapshotForKeyWithMeta(openAIAccountRuntimeStatsKey{
		accountID: accountID,
		kind:      openAIAccountRuntimeStatsKind(requiredImageCapability),
	})
	return errorRate, ttft, hasTTFT, sampleCount, ttftSampleCount, lastUpdated
}

func (s *openAIAccountRuntimeStats) snapshotForRoute(accountID int64, requestedModel string, transport OpenAIUpstreamTransport) (errorRate float64, ttft float64, hasTTFT bool) {
	if s == nil || accountID <= 0 {
		return 0, 0, false
	}
	if strings.TrimSpace(requestedModel) != "" || transport != "" {
		var found bool
		errorRate, ttft, hasTTFT, found = s.snapshotForKey(openAIAccountRuntimeStatsKey{
			accountID: accountID,
			kind:      "text",
			model:     requestedModel,
			transport: string(transport),
		})
		if found {
			return errorRate, ttft, hasTTFT
		}
	}
	return s.snapshotForRequest(accountID, "")
}

func (s *openAIAccountRuntimeStats) snapshotForRouteWithMeta(accountID int64, requestedModel string, transport OpenAIUpstreamTransport) (errorRate float64, ttft float64, hasTTFT bool, sampleCount int64, ttftSampleCount int64, lastUpdated time.Time) {
	if s == nil || accountID <= 0 {
		return 0, 0, false, 0, 0, time.Time{}
	}
	if strings.TrimSpace(requestedModel) != "" || transport != "" {
		var found bool
		errorRate, ttft, hasTTFT, found, sampleCount, ttftSampleCount, lastUpdated = s.snapshotForKeyWithMeta(openAIAccountRuntimeStatsKey{
			accountID: accountID,
			kind:      "text",
			model:     requestedModel,
			transport: string(transport),
		})
		if found {
			return errorRate, ttft, hasTTFT, sampleCount, ttftSampleCount, lastUpdated
		}
	}
	return s.snapshotForRequestWithMeta(accountID, "")
}

func (s *openAIAccountRuntimeStats) snapshotForKey(key openAIAccountRuntimeStatsKey) (errorRate float64, ttft float64, hasTTFT bool, found bool) {
	errorRate, ttft, hasTTFT, found, _, _, _ = s.snapshotForKeyWithMeta(key)
	return errorRate, ttft, hasTTFT, found
}

func (s *openAIAccountRuntimeStats) snapshotForKeyWithMeta(key openAIAccountRuntimeStatsKey) (errorRate float64, ttft float64, hasTTFT bool, found bool, sampleCount int64, ttftSampleCount int64, lastUpdated time.Time) {
	if s == nil || key.accountID <= 0 {
		return 0, 0, false, false, 0, 0, time.Time{}
	}
	key = normalizeOpenAIAccountRuntimeStatsKey(key)
	value, ok := s.accounts.Load(key)
	if !ok {
		return 0, 0, false, false, 0, 0, time.Time{}
	}
	stat, _ := value.(*openAIAccountRuntimeStat)
	if stat == nil {
		return 0, 0, false, false, 0, 0, time.Time{}
	}
	errorRate = clamp01(math.Float64frombits(stat.errorRateEWMABits.Load()))
	sampleCount = stat.sampleCount.Load()
	ttftSampleCount = stat.ttftSampleCount.Load()
	if updated := stat.lastUpdatedNano.Load(); updated > 0 {
		lastUpdated = time.Unix(0, updated)
	}
	ttftValue := math.Float64frombits(stat.ttftEWMABits.Load())
	if math.IsNaN(ttftValue) {
		return errorRate, 0, false, true, sampleCount, ttftSampleCount, lastUpdated
	}
	return errorRate, ttftValue, true, true, sampleCount, ttftSampleCount, lastUpdated
}

func (s *openAIAccountRuntimeStats) shouldTriggerUnknownExploration() bool {
	if s == nil || accountHealthUnknownExploreEvery == 0 {
		return false
	}
	return s.selectionCounter.Add(1)%accountHealthUnknownExploreEvery == 0
}

func (s *openAIAccountRuntimeStats) unknownExplorationDue(accountID int64, now time.Time) bool {
	if s == nil {
		return false
	}
	return accountHealthProbeDue(&s.unknownExploreAt, accountID, now, accountHealthUnknownExploreCooldown)
}

func (s *openAIAccountRuntimeStats) markUnknownExploration(accountID int64, now time.Time) {
	if s == nil {
		return
	}
	accountHealthMarkProbe(&s.unknownExploreAt, accountID, now)
}

func (s *openAIAccountRuntimeStats) degradedRecoveryDue(accountID int64, now time.Time) bool {
	if s == nil {
		return false
	}
	return accountHealthProbeDue(&s.degradedRecoveryAt, accountID, now, accountHealthDegradedRecoveryDelay)
}

func (s *openAIAccountRuntimeStats) markDegradedRecovery(accountID int64, now time.Time) {
	if s == nil {
		return
	}
	accountHealthMarkProbe(&s.degradedRecoveryAt, accountID, now)
}

func normalizeOpenAIAccountRuntimeStatsKey(key openAIAccountRuntimeStatsKey) openAIAccountRuntimeStatsKey {
	if key.kind == "" {
		key.kind = "text"
	}
	key.kind = strings.TrimSpace(key.kind)
	key.model = NormalizeOpenAICompatRequestedModel(strings.TrimSpace(key.model))
	key.transport = strings.TrimSpace(key.transport)
	return key
}

func (s *openAIAccountRuntimeStats) size() int {
	if s == nil {
		return 0
	}
	return int(s.accountCount.Load())
}

type defaultOpenAIAccountScheduler struct {
	service *OpenAIGatewayService
	metrics openAIAccountSchedulerMetrics
	stats   *openAIAccountRuntimeStats
}

func newDefaultOpenAIAccountScheduler(service *OpenAIGatewayService, stats *openAIAccountRuntimeStats) OpenAIAccountScheduler {
	if stats == nil {
		stats = newOpenAIAccountRuntimeStats()
	}
	return &defaultOpenAIAccountScheduler{
		service: service,
		stats:   stats,
	}
}

func (s *defaultOpenAIAccountScheduler) Select(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	decision := OpenAIAccountScheduleDecision{}
	start := time.Now()
	defer func() {
		decision.LatencyMs = time.Since(start).Milliseconds()
		if !IsOpenAIHealthProbeSessionHash(req.SessionHash) {
			s.metrics.recordSelect(decision)
		}
	}()

	previousResponseID := strings.TrimSpace(req.PreviousResponseID)
	if previousResponseID != "" {
		selection, err := s.service.selectAccountByPreviousResponseIDForCapability(
			ctx,
			req.GroupID,
			previousResponseID,
			req.RequestedModel,
			req.ExcludedIDs,
			req.RequiredCapability,
			req.RequireCompact,
		)
		if err != nil {
			return nil, decision, err
		}
		if selection != nil && selection.Account != nil {
			if !s.isAccountTransportCompatible(selection.Account, req.RequiredTransport) {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				selection = nil
			}
		}
		if selection != nil && selection.Account != nil {
			if !s.service.latestOpenAIAccountMatchesGroup(ctx, selection.Account, req.GroupID) {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				selection = nil
			}
		}
		if selection != nil && selection.Account != nil {
			if !openAIAccountMatchesLockedPriority(selection.Account, req.LockedPriority) {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				selection = nil
			}
		}
		if selection != nil && selection.Account != nil {
			if s.service.hasHigherPriorityOpenAIAccountAvailable(ctx, req.GroupID, selection.Account, req.RequestedModel, req.RequireCompact, req.RequiredCapability, req.RequiredImageCapability, req.RequiredTransport, req.RequestPlatform) {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				selection = nil
			}
		}
		if selection != nil && selection.Account != nil {
			if s.service.hasSamePriorityNonPoolOpenAIAccountAvailable(ctx, req.GroupID, selection.Account, req.RequestedModel, req.RequireCompact, req.RequiredCapability, req.RequiredImageCapability, req.RequiredTransport, req.RequestPlatform) {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				selection = nil
			}
		}
		if selection != nil && selection.Account != nil {
			decision.Layer = openAIAccountScheduleLayerPreviousResponse
			decision.StickyPreviousHit = true
			decision.SelectedAccountID = selection.Account.ID
			decision.SelectedAccountType = selection.Account.Type
			if req.SessionHash != "" {
				_ = s.service.BindStickySession(ctx, req.GroupID, req.SessionHash, selection.Account.ID)
			}
			return selection, decision, nil
		}
	}

	selection, stickyBusy, err := s.selectBySessionHash(ctx, req)
	if err != nil {
		return nil, decision, err
	}
	if selection != nil && selection.Account != nil {
		decision.Layer = openAIAccountScheduleLayerSessionSticky
		decision.StickySessionHit = true
		decision.SelectedAccountID = selection.Account.ID
		decision.SelectedAccountType = selection.Account.Type
		return selection, decision, nil
	}
	if stickyBusy {
		req.PreserveStickyBinding = true
	}

	selection, candidateCount, topK, loadSkew, err := s.selectByLoadBalance(ctx, req)
	decision.Layer = openAIAccountScheduleLayerLoadBalance
	decision.CandidateCount = candidateCount
	decision.TopK = topK
	decision.LoadSkew = loadSkew
	if err != nil {
		return nil, decision, err
	}
	if selection != nil && selection.Account != nil {
		decision.SelectedAccountID = selection.Account.ID
		decision.SelectedAccountType = selection.Account.Type
	}
	return selection, decision, nil
}

func (s *defaultOpenAIAccountScheduler) selectBySessionHash(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
) (*AccountSelectionResult, bool, error) {
	sessionHash := strings.TrimSpace(req.SessionHash)
	if s == nil || s.service == nil {
		return nil, false, nil
	}
	if sessionHash == "" && req.StickyAccountID <= 0 {
		return nil, false, nil
	}

	accountID := req.StickyAccountID
	if accountID <= 0 {
		if s.service.cache == nil {
			return nil, false, nil
		}
		var err error
		accountID, err = s.service.getStickySessionAccountID(ctx, req.GroupID, sessionHash)
		if err != nil || accountID <= 0 {
			return nil, false, nil
		}
	}
	if accountID <= 0 {
		return nil, false, nil
	}
	if req.ExcludedIDs != nil {
		if _, excluded := req.ExcludedIDs[accountID]; excluded {
			return nil, false, nil
		}
	}

	account, err := s.service.getSchedulableAccount(ctx, accountID)
	if err != nil || account == nil {
		if sessionHash != "" {
			_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, sessionHash)
		}
		return nil, false, nil
	}
	if IsOpenAIPromptCacheBoostAffinitySessionHash(sessionHash) &&
		!s.service.isOpenAIPromptCacheBoostAffinityHashUsableForAccount(sessionHash, account) {
		if sessionHash != "" {
			_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, sessionHash)
		}
		return nil, false, nil
	}
	if shouldClearStickySession(account, req.RequestedModel) ||
		!isOpenAICompatibleRequestPlatformAccount(account, req.RequestPlatform) ||
		!account.IsSchedulable() {
		if sessionHash != "" {
			_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, sessionHash)
		}
		return nil, false, nil
	}
	if !s.isAccountRequestCompatible(ctx, account, req) {
		return nil, false, nil
	}
	if !s.isAccountTransportCompatible(account, req.RequiredTransport) {
		if sessionHash != "" {
			_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, sessionHash)
		}
		return nil, false, nil
	}
	account = s.service.recheckSelectedOpenAIAccountFromDB(ctx, account, req.RequestedModel, req.RequireCompact, req.RequiredCapability, req.RequiredImageCapability, req.RequestPlatform)
	if account == nil || !s.service.latestOpenAIAccountMatchesGroup(ctx, account, req.GroupID) || !s.isAccountTransportCompatible(account, req.RequiredTransport) {
		if sessionHash != "" {
			_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, sessionHash)
		}
		return nil, false, nil
	}
	if IsOpenAIPromptCacheBoostAffinitySessionHash(sessionHash) &&
		!s.service.isOpenAIPromptCacheBoostAffinityHashUsableForAccount(sessionHash, account) {
		if sessionHash != "" {
			_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, sessionHash)
		}
		return nil, false, nil
	}
	if s.service.hasHigherPriorityOpenAIAccountAvailable(ctx, req.GroupID, account, req.RequestedModel, req.RequireCompact, req.RequiredCapability, req.RequiredImageCapability, req.RequiredTransport, req.RequestPlatform) {
		if sessionHash != "" {
			_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, sessionHash)
		}
		return nil, false, nil
	}
	if s.service.hasSamePriorityNonPoolOpenAIAccountAvailable(ctx, req.GroupID, account, req.RequestedModel, req.RequireCompact, req.RequiredCapability, req.RequiredImageCapability, req.RequiredTransport, req.RequestPlatform) {
		if sessionHash != "" {
			_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, sessionHash)
		}
		return nil, false, nil
	}
	result, acquireErr := s.service.tryAcquireAccountSlot(ctx, accountID, account.Concurrency)
	if acquireErr == nil && result != nil && result.Acquired {
		if sessionHash != "" {
			_ = s.service.refreshStickySessionTTL(ctx, req.GroupID, sessionHash, s.service.openAIStickySessionTTLForHash(sessionHash, s.service.openAIWSSessionStickyTTL()))
		}
		return &AccountSelectionResult{
			Account:     account,
			Acquired:    true,
			ReleaseFunc: result.ReleaseFunc,
		}, false, nil
	}
	return nil, acquireErr == nil, nil
}

func openAIStickyAccountMatchesGroup(account *Account, groupID *int64) bool {
	if account == nil {
		return false
	}
	if groupID == nil {
		return len(account.AccountGroups) == 0 && len(account.GroupIDs) == 0
	}
	for _, accountGroupID := range account.GroupIDs {
		if accountGroupID == *groupID {
			return true
		}
	}
	for _, accountGroup := range account.AccountGroups {
		if accountGroup.GroupID == *groupID {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) latestOpenAIAccountMatchesGroup(ctx context.Context, account *Account, groupID *int64) bool {
	if account == nil {
		return false
	}
	if groupID == nil {
		return openAIStickyAccountMatchesGroup(account, groupID)
	}
	if !accountHasGroupMetadata(account) {
		return true
	}
	if s != nil && s.accountRepo != nil {
		latest, err := s.accountRepo.GetByID(ctx, account.ID)
		if err == nil && latest != nil {
			if !accountHasGroupMetadata(latest) {
				return true
			}
			return openAIStickyAccountMatchesGroup(latest, groupID)
		}
	}
	return openAIStickyAccountMatchesGroup(account, groupID)
}

type openAIAccountCandidateScore struct {
	account         *Account
	loadInfo        *AccountLoadInfo
	loadInfoMissing bool
	score           float64
	errorRate       float64
	ttft            float64
	hasTTFT         bool
	sampleCount     int64
	ttftSampleCount int64
	lastUpdated     time.Time
	healthScore     float64
	hasHealthScore  bool
}

type openAIAccountCandidateHeap []openAIAccountCandidateScore

func (h openAIAccountCandidateHeap) Len() int {
	return len(h)
}

func (h openAIAccountCandidateHeap) Less(i, j int) bool {
	// 最小堆根节点保存“最差”候选，便于 O(log k) 维护 topK。
	return isOpenAIAccountCandidateBetter(h[j], h[i])
}

func (h openAIAccountCandidateHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *openAIAccountCandidateHeap) Push(x any) {
	candidate, ok := x.(openAIAccountCandidateScore)
	if !ok {
		panic("openAIAccountCandidateHeap: invalid element type")
	}
	*h = append(*h, candidate)
}

func (h *openAIAccountCandidateHeap) Pop() any {
	old := *h
	n := len(old)
	last := old[n-1]
	*h = old[:n-1]
	return last
}

func isOpenAIAccountCandidateBetter(left openAIAccountCandidateScore, right openAIAccountCandidateScore) bool {
	if left.score != right.score {
		return left.score > right.score
	}
	if left.account.Priority != right.account.Priority {
		return left.account.Priority < right.account.Priority
	}
	if left.loadInfoMissing != right.loadInfoMissing {
		return !left.loadInfoMissing
	}
	return left.account.ID < right.account.ID
}

func openAIAccountMatchesLockedPriority(account *Account, lockedPriority int) bool {
	return lockedPriority < 0 || (account != nil && account.Priority == lockedPriority)
}

func selectTopKOpenAICandidates(candidates []openAIAccountCandidateScore, topK int) []openAIAccountCandidateScore {
	if len(candidates) == 0 {
		return nil
	}
	if topK <= 0 {
		topK = 1
	}
	if topK >= len(candidates) {
		ranked := append([]openAIAccountCandidateScore(nil), candidates...)
		sort.Slice(ranked, func(i, j int) bool {
			return isOpenAIAccountCandidateBetter(ranked[i], ranked[j])
		})
		return ranked
	}

	best := make(openAIAccountCandidateHeap, 0, topK)
	for _, candidate := range candidates {
		if len(best) < topK {
			heap.Push(&best, candidate)
			continue
		}
		if isOpenAIAccountCandidateBetter(candidate, best[0]) {
			best[0] = candidate
			heap.Fix(&best, 0)
		}
	}

	ranked := make([]openAIAccountCandidateScore, len(best))
	copy(ranked, best)
	sort.Slice(ranked, func(i, j int) bool {
		return isOpenAIAccountCandidateBetter(ranked[i], ranked[j])
	})
	return ranked
}

type openAISelectionRNG struct {
	state uint64
}

func newOpenAISelectionRNG(seed uint64) openAISelectionRNG {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return openAISelectionRNG{state: seed}
}

func (r *openAISelectionRNG) nextUint64() uint64 {
	// xorshift64*
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 2685821657736338717
}

func (r *openAISelectionRNG) nextFloat64() float64 {
	// [0,1)
	return float64(r.nextUint64()>>11) / (1 << 53)
}

func deriveOpenAISelectionSeed(req OpenAIAccountScheduleRequest) uint64 {
	hasher := fnv.New64a()
	writeValue := func(value string) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return
		}
		_, _ = hasher.Write([]byte(trimmed))
		_, _ = hasher.Write([]byte{0})
	}

	writeValue(req.SessionHash)
	writeValue(req.PreviousResponseID)
	writeValue(req.RequestedModel)
	if req.GroupID != nil {
		_, _ = hasher.Write([]byte(strconv.FormatInt(*req.GroupID, 10)))
	}

	seed := hasher.Sum64()
	// 对“无会话锚点”的纯负载均衡请求引入时间熵，避免固定命中同一账号。
	if strings.TrimSpace(req.SessionHash) == "" && strings.TrimSpace(req.PreviousResponseID) == "" {
		seed ^= uint64(time.Now().UnixNano())
	}
	if seed == 0 {
		seed = uint64(time.Now().UnixNano()) ^ 0x9e3779b97f4a7c15
	}
	return seed
}

func buildOpenAIWeightedSelectionOrder(
	candidates []openAIAccountCandidateScore,
	req OpenAIAccountScheduleRequest,
) []openAIAccountCandidateScore {
	if len(candidates) <= 1 {
		return append([]openAIAccountCandidateScore(nil), candidates...)
	}

	pool := append([]openAIAccountCandidateScore(nil), candidates...)
	weights := make([]float64, len(pool))
	minScore := pool[0].score
	for i := 1; i < len(pool); i++ {
		if pool[i].score < minScore {
			minScore = pool[i].score
		}
	}
	for i := range pool {
		// 将 top-K 分值平移到正区间，避免“单一最高分账号”长期垄断。
		weight := (pool[i].score - minScore) + 1.0
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight <= 0 {
			weight = 1.0
		}
		weights[i] = weight
	}

	order := make([]openAIAccountCandidateScore, 0, len(pool))
	rng := newOpenAISelectionRNG(deriveOpenAISelectionSeed(req))
	for len(pool) > 0 {
		total := 0.0
		for _, w := range weights {
			total += w
		}

		selectedIdx := 0
		if total > 0 {
			r := rng.nextFloat64() * total
			acc := 0.0
			for i, w := range weights {
				acc += w
				if r <= acc {
					selectedIdx = i
					break
				}
			}
		} else {
			selectedIdx = int(rng.nextUint64() % uint64(len(pool)))
		}

		order = append(order, pool[selectedIdx])
		pool = append(pool[:selectedIdx], pool[selectedIdx+1:]...)
		weights = append(weights[:selectedIdx], weights[selectedIdx+1:]...)
	}
	return order
}

func (s *defaultOpenAIAccountScheduler) buildOpenAIAccountLoadPlan(
	req OpenAIAccountScheduleRequest,
	filtered []*Account,
	loadMap map[int64]*AccountLoadInfo,
) openAIAccountLoadPlan {
	allCandidates := make([]openAIAccountCandidateScore, 0, len(filtered))
	for _, account := range filtered {
		loadInfo := loadMap[account.ID]
		loadInfoMissing := loadInfo == nil
		if loadInfo == nil {
			loadInfo = &AccountLoadInfo{AccountID: account.ID}
		}
		errorRate, ttft, hasTTFT := 0.0, 0.0, false
		sampleCount, ttftSampleCount, lastUpdated := int64(0), int64(0), time.Time{}
		if s.stats != nil {
			if req.RequiredImageCapability != "" {
				errorRate, ttft, hasTTFT, sampleCount, ttftSampleCount, lastUpdated = s.stats.snapshotForRequestWithMeta(account.ID, req.RequiredImageCapability)
			} else {
				errorRate, ttft, hasTTFT, sampleCount, ttftSampleCount, lastUpdated = s.stats.snapshotForRouteWithMeta(account.ID, req.RequestedModel, req.RequiredTransport)
			}
		}
		allCandidates = append(allCandidates, openAIAccountCandidateScore{
			account:         account,
			loadInfo:        loadInfo,
			loadInfoMissing: loadInfoMissing,
			errorRate:       errorRate,
			ttft:            ttft,
			hasTTFT:         hasTTFT,
			sampleCount:     sampleCount,
			ttftSampleCount: ttftSampleCount,
			lastUpdated:     lastUpdated,
		})
	}

	candidates := allCandidates
	staleSnapshotCompactRetry := make([]openAIAccountCandidateScore, 0, len(allCandidates))
	if req.RequireCompact {
		candidates = make([]openAIAccountCandidateScore, 0, len(allCandidates))
		for _, candidate := range allCandidates {
			if openAICompactSupportTier(candidate.account) == 0 {
				staleSnapshotCompactRetry = append(staleSnapshotCompactRetry, candidate)
				continue
			}
			candidates = append(candidates, candidate)
		}
	}

	plan := openAIAccountLoadPlan{
		allCandidates:             allCandidates,
		candidates:                candidates,
		staleSnapshotCompactRetry: staleSnapshotCompactRetry,
		candidateCount:            len(candidates),
	}
	if len(candidates) == 0 {
		plan.selectionOrder = s.buildOpenAISelectionOrder(req, plan)
		return plan
	}

	minPriority, maxPriority := candidates[0].account.Priority, candidates[0].account.Priority
	maxWaiting := 1
	loadRateSum := 0.0
	loadRateSumSquares := 0.0
	minTTFT, maxTTFT := 0.0, 0.0
	hasTTFTSample := false
	minResetRemaining, maxResetRemaining := 0.0, 0.0
	hasResetSample := false
	weights := s.service.openAIWSSchedulerWeights()
	preferResetScore := weights.Reset > 0 || s.service.schedulingConfig().PreferSoonestReset
	now := time.Time{}
	if preferResetScore || weights.QuotaHeadroom > 0 {
		now = time.Now()
	}
	for _, candidate := range candidates {
		if candidate.account.Priority < minPriority {
			minPriority = candidate.account.Priority
		}
		if candidate.account.Priority > maxPriority {
			maxPriority = candidate.account.Priority
		}
		if candidate.loadInfo.WaitingCount > maxWaiting {
			maxWaiting = candidate.loadInfo.WaitingCount
		}
		if candidate.hasTTFT && candidate.ttft > 0 {
			if !hasTTFTSample {
				minTTFT, maxTTFT = candidate.ttft, candidate.ttft
				hasTTFTSample = true
			} else {
				if candidate.ttft < minTTFT {
					minTTFT = candidate.ttft
				}
				if candidate.ttft > maxTTFT {
					maxTTFT = candidate.ttft
				}
			}
		}
		if preferResetScore {
			if resetAt := futureSessionWindowEnd(candidate.account, now); resetAt != nil {
				remaining := resetAt.Sub(now).Seconds()
				if !hasResetSample {
					minResetRemaining, maxResetRemaining = remaining, remaining
					hasResetSample = true
				} else {
					if remaining < minResetRemaining {
						minResetRemaining = remaining
					}
					if remaining > maxResetRemaining {
						maxResetRemaining = remaining
					}
				}
			}
		}
		loadRate := float64(candidate.loadInfo.LoadRate)
		loadRateSum += loadRate
		loadRateSumSquares += loadRate * loadRate
	}
	plan.loadSkew = calcLoadSkewByMoments(loadRateSum, loadRateSumSquares, len(candidates))

	for i := range candidates {
		item := &candidates[i]
		priorityFactor := 1.0
		if maxPriority > minPriority {
			priorityFactor = 1 - float64(item.account.Priority-minPriority)/float64(maxPriority-minPriority)
		}
		loadFactor := 1 - clamp01(float64(item.loadInfo.LoadRate)/100.0)
		queueFactor := 1 - clamp01(float64(item.loadInfo.WaitingCount)/float64(maxWaiting))
		errorFactor := 1 - clamp01(item.errorRate)
		ttftFactor := 0.5
		if item.hasTTFT && hasTTFTSample && maxTTFT > minTTFT {
			ttftFactor = 1 - clamp01((item.ttft-minTTFT)/(maxTTFT-minTTFT))
		}
		resetFactor := 0.5
		if preferResetScore {
			if resetAt := futureSessionWindowEnd(item.account, now); resetAt != nil {
				resetFactor = 1.0
				if hasResetSample && maxResetRemaining > minResetRemaining {
					resetFactor = 1 - clamp01((resetAt.Sub(now).Seconds()-minResetRemaining)/(maxResetRemaining-minResetRemaining))
				}
			}
		}
		quotaHeadroomFactor := 0.0
		if weights.QuotaHeadroom > 0 {
			quotaHeadroomFactor = openAIQuotaHeadroomFactor(item.account, now)
		}

		item.score = weights.Priority*priorityFactor +
			weights.Load*loadFactor +
			weights.Queue*queueFactor +
			weights.ErrorRate*errorFactor +
			weights.TTFT*ttftFactor +
			weights.Reset*resetFactor +
			weights.QuotaHeadroom*quotaHeadroomFactor
	}
	plan.candidates = candidates

	plan.topK = s.service.openAIWSLBTopK()
	if plan.topK > len(candidates) {
		plan.topK = len(candidates)
	}
	if plan.topK <= 0 {
		plan.topK = 1
	}

	plan.selectionOrder = s.buildOpenAISelectionOrder(req, plan)
	return plan
}

func (s *defaultOpenAIAccountScheduler) buildOpenAISelectionOrder(
	req OpenAIAccountScheduleRequest,
	plan openAIAccountLoadPlan,
) []openAIAccountCandidateScore {
	buildSelectionOrder := func(pool []openAIAccountCandidateScore) []openAIAccountCandidateScore {
		if len(pool) == 0 {
			return nil
		}
		return s.buildStrictPrioritySelectionOrderForSession(pool, req.RequestedModel, req.SessionHash)
	}

	if req.RequireCompact {
		supported := make([]openAIAccountCandidateScore, 0, len(plan.candidates))
		unknown := make([]openAIAccountCandidateScore, 0, len(plan.candidates))
		for _, candidate := range plan.candidates {
			switch openAICompactSupportTier(candidate.account) {
			case 2:
				supported = append(supported, candidate)
			case 1:
				unknown = append(unknown, candidate)
			}
		}
		selectionOrder := make([]openAIAccountCandidateScore, 0, len(plan.allCandidates))
		selectionOrder = append(selectionOrder, buildSelectionOrder(supported)...)
		selectionOrder = append(selectionOrder, buildSelectionOrder(unknown)...)
		if len(plan.staleSnapshotCompactRetry) > 0 && s.service.schedulerSnapshot != nil {
			selectionOrder = append(selectionOrder, sortOpenAICompactRetryCandidates(plan.staleSnapshotCompactRetry)...)
		}
		return selectionOrder
	}

	return buildSelectionOrder(plan.candidates)
}

func (s *defaultOpenAIAccountScheduler) buildStrictPrioritySelectionOrder(pool []openAIAccountCandidateScore, requestedModel string) []openAIAccountCandidateScore {
	return s.buildStrictPrioritySelectionOrderForSession(pool, requestedModel, "")
}

func (s *defaultOpenAIAccountScheduler) buildStrictPrioritySelectionOrderForSession(pool []openAIAccountCandidateScore, requestedModel string, sessionHash string) []openAIAccountCandidateScore {
	if len(pool) == 0 {
		return nil
	}
	active := make([]openAIAccountCandidateScore, 0, len(pool))
	probeDue := make([]openAIAccountCandidateScore, 0)
	for _, candidate := range pool {
		if s != nil && s.service != nil && s.service.isOpenAIPoolAccountSoftCooling(candidate.account) {
			if s.service.isOpenAIPoolAccountSoftCooldownDue(candidate.account) {
				if s.service.clearOpenAIPoolSoftCooldownIfRecoveryProbeDisabled(context.Background(), candidate.account, requestedModel) {
					active = append(active, candidate)
					continue
				}
				probeDue = append(probeDue, candidate)
			}
			continue
		}
		active = append(active, candidate)
	}

	ordered := make([]openAIAccountCandidateScore, 0, len(pool))
	ordered = append(ordered, s.sortOpenAIStrictPriorityCandidatesForSession(active, sessionHash)...)
	if len(probeDue) > 0 && s != nil && s.service != nil {
		for _, candidate := range probeDue {
			s.service.maybeStartOpenAIPoolRecoveryProbe(context.Background(), candidate.account, requestedModel)
		}
	}
	return ordered
}

func (s *defaultOpenAIAccountScheduler) sortOpenAIPoolCooldownProbeCandidates(pool []openAIAccountCandidateScore) []openAIAccountCandidateScore {
	if len(pool) == 0 {
		return nil
	}
	ordered := s.sortOpenAIStrictPriorityCandidates(pool)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.account.Priority != b.account.Priority {
			return a.account.Priority < b.account.Priority
		}
		aUntil, aCooling := time.Time{}, false
		bUntil, bCooling := time.Time{}, false
		if s != nil && s.service != nil {
			aUntil, aCooling = s.service.openAIPoolAccountSoftCooldownUntil(a.account)
			bUntil, bCooling = s.service.openAIPoolAccountSoftCooldownUntil(b.account)
		}
		if aCooling != bCooling {
			return !aCooling
		}
		if aCooling && bCooling && !aUntil.Equal(bUntil) {
			return aUntil.Before(bUntil)
		}
		switch {
		case a.account.LastUsedAt == nil && b.account.LastUsedAt != nil:
			return true
		case a.account.LastUsedAt != nil && b.account.LastUsedAt == nil:
			return false
		case a.account.LastUsedAt != nil && b.account.LastUsedAt != nil:
			if !a.account.LastUsedAt.Equal(*b.account.LastUsedAt) {
				return a.account.LastUsedAt.Before(*b.account.LastUsedAt)
			}
		}
		if a.loadInfoMissing != b.loadInfoMissing {
			return !a.loadInfoMissing
		}
		if a.loadInfo.LoadRate != b.loadInfo.LoadRate {
			return a.loadInfo.LoadRate < b.loadInfo.LoadRate
		}
		return a.account.ID < b.account.ID
	})
	return ordered
}

func sortOpenAIStrictPriorityCandidates(pool []openAIAccountCandidateScore) []openAIAccountCandidateScore {
	return sortOpenAIStrictPriorityCandidatesWithReset(pool, false)
}

func (s *defaultOpenAIAccountScheduler) sortOpenAIStrictPriorityCandidates(pool []openAIAccountCandidateScore) []openAIAccountCandidateScore {
	return s.sortOpenAIStrictPriorityCandidatesForSession(pool, "")
}

func (s *defaultOpenAIAccountScheduler) sortOpenAIStrictPriorityCandidatesForSession(pool []openAIAccountCandidateScore, sessionHash string) []openAIAccountCandidateScore {
	preferSoonestReset := false
	if s != nil && s.service != nil {
		weights := s.service.openAIWSSchedulerWeights()
		preferSoonestReset = weights.Reset > 0 || s.service.schedulingConfig().PreferSoonestReset
	}
	ordered := sortOpenAIStrictPriorityCandidatesWithResetAndSession(pool, preferSoonestReset, sessionHash)
	return ordered
}

func hasKnownOpenAIHealthSample(candidates []openAIAccountCandidateScore) bool {
	for _, candidate := range candidates {
		if accountHealthHasKnownSamples(candidate.sampleCount, candidate.ttftSampleCount, candidate.errorRate) {
			return true
		}
	}
	return false
}

func openAIHealthProbeTopLayer(candidates []openAIAccountCandidateScore) []openAIAccountCandidateScore {
	if len(candidates) == 0 || candidates[0].account == nil {
		return nil
	}
	priority := candidates[0].account.Priority
	poolMode := candidates[0].account.IsPoolMode()
	end := 0
	for end < len(candidates) {
		account := candidates[end].account
		if account == nil || account.Priority != priority || account.IsPoolMode() != poolMode {
			break
		}
		end++
	}
	return candidates[:end]
}

func selectOpenAIUnknownExplorationIndex(candidates []openAIAccountCandidateScore, stats *openAIAccountRuntimeStats, now time.Time) int {
	if stats == nil || !hasKnownOpenAIHealthSample(candidates) || !stats.shouldTriggerUnknownExploration() {
		return -1
	}
	for i, candidate := range candidates {
		if candidate.account == nil || accountHealthHasKnownSamples(candidate.sampleCount, candidate.ttftSampleCount, candidate.errorRate) {
			continue
		}
		if !stats.unknownExplorationDue(candidate.account.ID, now) {
			continue
		}
		stats.markUnknownExploration(candidate.account.ID, now)
		return i
	}
	return -1
}

func selectOpenAIDegradedRecoveryIndex(candidates []openAIAccountCandidateScore, stats *openAIAccountRuntimeStats, now time.Time) int {
	if stats == nil || !hasKnownOpenAIHealthSample(candidates) {
		return -1
	}
	bestScore := -1.0
	for _, candidate := range candidates {
		if candidate.healthScore > bestScore {
			bestScore = candidate.healthScore
		}
	}
	if bestScore < 0 {
		return -1
	}
	for i, candidate := range candidates {
		if candidate.account == nil || !accountHealthHasKnownSamples(candidate.sampleCount, candidate.ttftSampleCount, candidate.errorRate) {
			continue
		}
		if candidate.healthScore >= bestScore-accountHealthScoreBandThreshold {
			continue
		}
		if accountHealthSampleRecentlyUpdated(candidate.lastUpdated, now, accountHealthDegradedRecoveryDelay) {
			continue
		}
		if !stats.degradedRecoveryDue(candidate.account.ID, now) {
			continue
		}
		stats.markDegradedRecovery(candidate.account.ID, now)
		return i
	}
	return -1
}

func prioritizeOpenAIHealthProbeCandidate(candidates []openAIAccountCandidateScore, stats *openAIAccountRuntimeStats, now time.Time) []openAIAccountCandidateScore {
	if len(candidates) <= 1 || stats == nil {
		return candidates
	}
	topLayer := openAIHealthProbeTopLayer(candidates)
	if len(topLayer) <= 1 {
		return candidates
	}
	idx := selectOpenAIUnknownExplorationIndex(topLayer, stats, now)
	if idx < 0 {
		idx = selectOpenAIDegradedRecoveryIndex(topLayer, stats, now)
	}
	if idx <= 0 {
		return candidates
	}
	ordered := append([]openAIAccountCandidateScore(nil), candidates...)
	selected := ordered[idx]
	copy(ordered[1:idx+1], ordered[0:idx])
	ordered[0] = selected
	return ordered
}

func sortOpenAIStrictPriorityCandidatesWithReset(pool []openAIAccountCandidateScore, preferSoonestReset bool) []openAIAccountCandidateScore {
	return sortOpenAIStrictPriorityCandidatesWithResetAndSession(pool, preferSoonestReset, "")
}

func sortOpenAIStrictPriorityCandidatesWithResetAndSession(pool []openAIAccountCandidateScore, preferSoonestReset bool, sessionHash string) []openAIAccountCandidateScore {
	if len(pool) == 0 {
		return nil
	}
	now := time.Time{}
	if preferSoonestReset {
		now = time.Now()
	}
	ordered := append([]openAIAccountCandidateScore(nil), pool...)
	healthScores := buildOpenAIAccountCandidateHealthScores(ordered)
	for i := range ordered {
		if ordered[i].account == nil {
			continue
		}
		ordered[i].healthScore = healthScores[ordered[i].account.ID]
		ordered[i].hasHealthScore = true
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.account.Priority != b.account.Priority {
			return a.account.Priority < b.account.Priority
		}
		if less, ok := nonPoolAccountBeforePool(a.account, b.account); ok {
			return less
		}
		if less, ok := openAIHealthScoreLess(healthScores[a.account.ID], healthScores[b.account.ID]); ok {
			return less
		}
		if preferSoonestReset {
			if less, ok := accountSoonestResetLess(a.account, b.account, now); ok {
				return less
			}
		}
		if a.errorRate != b.errorRate {
			return a.errorRate < b.errorRate
		}
		switch {
		case a.account.LastUsedAt == nil && b.account.LastUsedAt != nil:
			return true
		case a.account.LastUsedAt != nil && b.account.LastUsedAt == nil:
			return false
		case a.account.LastUsedAt != nil && b.account.LastUsedAt != nil:
			if !a.account.LastUsedAt.Equal(*b.account.LastUsedAt) {
				return a.account.LastUsedAt.Before(*b.account.LastUsedAt)
			}
		}
		if a.hasTTFT != b.hasTTFT {
			return a.hasTTFT
		}
		if a.hasTTFT && a.ttft != b.ttft {
			return a.ttft < b.ttft
		}
		return a.account.ID < b.account.ID
	})
	shuffleOpenAIStrictPriorityTiesWithReset(ordered, preferSoonestReset)
	prioritizeOpenAIPromptCacheUpstreamStrictTies(ordered, sessionHash, preferSoonestReset)
	return ordered
}

func buildOpenAIAccountCandidateHealthScores(pool []openAIAccountCandidateScore) map[int64]float64 {
	if len(pool) == 0 {
		return nil
	}
	minTTFT, maxTTFT := 0.0, 0.0
	hasTTFTSample := false
	for _, candidate := range pool {
		if candidate.hasTTFT && candidate.ttft > 0 {
			if !hasTTFTSample || candidate.ttft < minTTFT {
				minTTFT = candidate.ttft
			}
			if !hasTTFTSample || candidate.ttft > maxTTFT {
				maxTTFT = candidate.ttft
			}
			hasTTFTSample = true
		}
	}

	scores := make(map[int64]float64, len(pool))
	for _, candidate := range pool {
		if candidate.account == nil {
			continue
		}
		scores[candidate.account.ID] = openAIAccountRuntimeHealthScore(candidate.errorRate, candidate.ttft, candidate.hasTTFT, minTTFT, maxTTFT, hasTTFTSample)
	}
	return scores
}

func openAIAccountRuntimeHealthScore(errorRate float64, ttft float64, hasTTFT bool, minTTFT float64, maxTTFT float64, hasTTFTSample bool) float64 {
	errorFactor := 1 - clamp01(errorRate)
	ttftFactor := accountHealthUnknownTTFTScore
	if hasTTFT {
		ttftFactor = 1
		if hasTTFTSample && maxTTFT > minTTFT {
			ttftSpread := minTTFT * 2
			if ttftSpread < 300 {
				ttftSpread = 300
			}
			ttftFactor = 1 - clamp01((ttft-minTTFT)/ttftSpread)
		}
	}
	score := accountHealthErrorWeight*errorFactor + accountHealthTTFTWeight*ttftFactor
	if !hasTTFT && score > accountHealthUnknownScore {
		return accountHealthUnknownScore
	}
	return score
}

func openAIHealthScoreLess(aScore, bScore float64) (bool, bool) {
	diff := aScore - bScore
	if math.Abs(diff) <= accountHealthScoreBandThreshold {
		return false, false
	}
	return diff > 0, true
}

func sortOpenAICompactRetryCandidates(pool []openAIAccountCandidateScore) []openAIAccountCandidateScore {
	if len(pool) == 0 {
		return nil
	}
	return sortOpenAIStrictPriorityCandidates(pool)
}

func (s *defaultOpenAIAccountScheduler) tryAcquireOpenAISelectionOrder(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	selectionOrder []openAIAccountCandidateScore,
) (*AccountSelectionResult, bool, error) {
	if result, compactBlocked, attempted, err := s.tryAcquireOpenAISelectionOrderWithArbiter(ctx, req, selectionOrder); err != nil || result != nil {
		return result, compactBlocked, err
	} else if attempted {
		if req.UserID > 0 && req.UserConcurrency > 0 {
			return nil, compactBlocked, nil
		}
		// Small-window arbitration found no immediately available slot. Continue
		// through the existing per-account path so fresh-load retry and wait-plan
		// semantics stay unchanged.
		_ = compactBlocked
	} else if req.UserID > 0 && req.UserConcurrency > 0 {
		return nil, false, nil
	}

	compactBlocked := false
	for i := 0; i < len(selectionOrder); i++ {
		candidate := selectionOrder[i]
		fresh := s.service.resolveFreshSchedulableOpenAIAccount(ctx, candidate.account, req.RequestedModel, false, req.RequiredCapability, req.RequiredImageCapability, req.RequestPlatform)
		if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
			continue
		}
		fresh = s.service.recheckSelectedOpenAIAccountFromDB(ctx, fresh, req.RequestedModel, false, req.RequiredCapability, req.RequiredImageCapability, req.RequestPlatform)
		if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
			continue
		}
		if req.RequireCompact && openAICompactSupportTier(fresh) == 0 {
			compactBlocked = true
			continue
		}
		result, acquireErr := s.service.tryAcquireAccountSlot(ctx, fresh.ID, fresh.Concurrency)
		if acquireErr != nil {
			return nil, compactBlocked, acquireErr
		}
		if result != nil && result.Acquired {
			if req.SessionHash != "" && !req.PreserveStickyBinding {
				_ = s.service.BindStickySession(ctx, req.GroupID, req.SessionHash, fresh.ID)
			}
			return &AccountSelectionResult{
				Account:     fresh,
				Acquired:    true,
				ReleaseFunc: result.ReleaseFunc,
			}, compactBlocked, nil
		}
	}
	return nil, compactBlocked, nil
}

func (s *defaultOpenAIAccountScheduler) tryAcquireOpenAISelectionOrderWithArbiter(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	selectionOrder []openAIAccountCandidateScore,
) (*AccountSelectionResult, bool, bool, error) {
	if s == nil || s.service == nil || !s.service.openAIAccountSlotArbiterEnabled() || s.service.concurrencyService == nil {
		return nil, false, false, nil
	}
	maxCandidates := s.service.openAIAccountSlotArbiterMaxCandidates()
	if maxCandidates <= 0 {
		return nil, false, false, nil
	}

	compactBlocked := false
	freshByID := make(map[int64]*Account, maxCandidates)
	candidates := make([]AccountSlotCandidate, 0, maxCandidates)
	for i := 0; i < len(selectionOrder) && len(candidates) < maxCandidates; i++ {
		candidate := selectionOrder[i]
		fresh := s.service.resolveFreshSchedulableOpenAIAccount(ctx, candidate.account, req.RequestedModel, false, req.RequiredCapability, req.RequiredImageCapability, req.RequestPlatform)
		if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
			continue
		}
		fresh = s.service.recheckSelectedOpenAIAccountFromDB(ctx, fresh, req.RequestedModel, false, req.RequiredCapability, req.RequiredImageCapability, req.RequestPlatform)
		if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
			continue
		}
		if req.RequireCompact && openAICompactSupportTier(fresh) == 0 {
			compactBlocked = true
			continue
		}
		freshByID[fresh.ID] = fresh
		candidates = append(candidates, AccountSlotCandidate{
			AccountID:      fresh.ID,
			MaxConcurrency: fresh.Concurrency,
		})
	}
	if len(candidates) == 0 {
		return nil, compactBlocked, false, nil
	}

	var (
		accountReleaseFunc func()
		userReleaseFunc    func()
		selectedAccountID  int64
		acquired           bool
	)
	if req.UserID > 0 && req.UserConcurrency > 0 {
		userArbitration, err := s.service.concurrencyService.AcquireFirstAvailableUserAccountSlots(ctx, req.UserID, req.UserConcurrency, candidates)
		if err != nil {
			slog.Warn("openai user/account slot arbiter failed, fallback to per-account acquire",
				"error", err,
				"candidate_count", len(candidates),
			)
			return nil, compactBlocked, false, nil
		}
		if userArbitration == nil {
			return nil, compactBlocked, false, nil
		}
		acquired = userArbitration.Acquired
		selectedAccountID = userArbitration.AccountID
		accountReleaseFunc = userArbitration.ReleaseFunc
		userReleaseFunc = userArbitration.UserReleaseFunc
	} else {
		arbitration, err := s.service.concurrencyService.AcquireFirstAvailableAccountSlot(ctx, candidates)
		if err != nil {
			slog.Warn("openai account slot arbiter failed, fallback to per-account acquire",
				"error", err,
				"candidate_count", len(candidates),
			)
			return nil, compactBlocked, false, nil
		}
		if arbitration == nil {
			return nil, compactBlocked, false, nil
		}
		acquired = arbitration.Acquired
		selectedAccountID = arbitration.AccountID
		accountReleaseFunc = arbitration.ReleaseFunc
	}
	if !acquired || selectedAccountID <= 0 {
		return nil, compactBlocked, true, nil
	}
	fresh := freshByID[selectedAccountID]
	if fresh == nil {
		if accountReleaseFunc != nil {
			accountReleaseFunc()
		}
		if userReleaseFunc != nil {
			userReleaseFunc()
		}
		return nil, compactBlocked, true, nil
	}
	if req.SessionHash != "" && !req.PreserveStickyBinding {
		_ = s.service.BindStickySession(ctx, req.GroupID, req.SessionHash, fresh.ID)
	}
	return &AccountSelectionResult{
		Account:         fresh,
		Acquired:        true,
		ReleaseFunc:     accountReleaseFunc,
		UserReleaseFunc: userReleaseFunc,
	}, compactBlocked, true, nil
}

func (s *defaultOpenAIAccountScheduler) selectByLoadBalance(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
) (*AccountSelectionResult, int, int, float64, error) {
	accounts, err := s.service.listSchedulableAccountsForPlatform(ctx, req.GroupID, req.RequestPlatform)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	if len(accounts) == 0 {
		return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false)
	}

	// require_privacy_set: 获取分组信息
	var schedGroup *Group
	if req.GroupID != nil && s.service.schedulerSnapshot != nil {
		schedGroup, _ = s.service.schedulerSnapshot.GetGroupByID(ctx, *req.GroupID)
	}

	baseFiltered := make([]*Account, 0, len(accounts))
	filtered := make([]*Account, 0, len(accounts))
	loadReq := make([]AccountWithConcurrency, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if req.ExcludedIDs != nil {
			if _, excluded := req.ExcludedIDs[account.ID]; excluded {
				continue
			}
		}
		if !account.IsSchedulable() || !isOpenAICompatibleRequestPlatformAccount(account, req.RequestPlatform) {
			continue
		}
		if s.service.isOpenAIAccountRuntimeBlocked(account) {
			continue
		}
		// require_privacy_set: 跳过 privacy 未设置的账号并标记异常
		if schedGroup != nil && schedGroup.RequirePrivacySet && !account.IsPrivacySet() {
			s.service.BlockAccountScheduling(account, time.Time{}, "privacy_not_set")
			_ = s.service.accountRepo.SetError(ctx, account.ID,
				fmt.Sprintf("Privacy not set, required by group [%s]", schedGroup.Name))
			continue
		}
		if !openAIAccountMatchesLockedPriority(account, req.LockedPriority) {
			continue
		}
		if s != nil && s.service != nil && !parentHealthyForShadow(account, s.service.parentAccountLookup(ctx)) {
			continue
		}
		if paused, _ := shouldAutoPauseOpenAIAccountByQuota(ctx, account); paused {
			continue
		}
		if !s.isAccountTransportCompatible(account, req.RequiredTransport) {
			continue
		}
		if s.shouldIncludeOpenAIAccountInPriorityBase(ctx, account, req.RequestedModel) {
			baseFiltered = append(baseFiltered, account)
		}
		if !s.isAccountRequestCompatible(ctx, account, req) {
			continue
		}
		filtered = append(filtered, account)
		loadReq = append(loadReq, AccountWithConcurrency{
			ID:             account.ID,
			MaxConcurrency: account.EffectiveLoadFactor(),
		})
	}
	if groupStrictModelPriorityOnMismatch(schedGroup) && !openAILowestBasePrioritySupportsRequestedModel(baseFiltered, req.RequestedModel) {
		filtered = nil
	}
	if len(filtered) == 0 {
		return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false)
	}

	loadMap := map[int64]*AccountLoadInfo{}
	if s.service.concurrencyService != nil && !s.service.openAIAccountSlotArbiterEnabled() {
		if batchLoad, loadErr := s.service.concurrencyService.GetAccountsLoadBatch(ctx, loadReq); loadErr == nil {
			loadMap = batchLoad
		}
	}

	plan := s.buildOpenAIAccountLoadPlan(req, filtered, loadMap)
	candidateCount := plan.candidateCount
	topK := plan.topK
	loadSkew := plan.loadSkew
	selectionOrder := plan.selectionOrder
	selectionOrder = s.service.prioritizeOpenAIPromptCacheWarmCandidates(ctx, req, selectionOrder)
	if req.RequireCompact && len(plan.candidates) == 0 && len(plan.staleSnapshotCompactRetry) == 0 {
		return nil, 0, 0, 0, ErrNoAvailableCompactAccounts
	}
	if req.RequireCompact && len(selectionOrder) == 0 && s.service.schedulerSnapshot == nil {
		return nil, candidateCount, topK, loadSkew, ErrNoAvailableCompactAccounts
	}
	if len(selectionOrder) == 0 {
		return nil, candidateCount, topK, loadSkew, noAvailableOpenAISelectionError(req.RequestedModel, req.RequireCompact && len(plan.allCandidates) > 0)
	}

	result, compactBlocked, acquireErr := s.tryAcquireOpenAISelectionOrder(ctx, req, selectionOrder)
	if acquireErr != nil {
		return nil, candidateCount, topK, loadSkew, acquireErr
	}
	if result != nil {
		return result, candidateCount, topK, loadSkew, nil
	}
	if req.UserID > 0 && req.UserConcurrency > 0 {
		return nil, candidateCount, topK, loadSkew, nil
	}

	if s.service.concurrencyService != nil {
		if freshLoadMap, loadErr := s.service.concurrencyService.GetAccountsLoadBatchFresh(ctx, loadReq); loadErr == nil {
			freshPlan := s.buildOpenAIAccountLoadPlan(req, filtered, freshLoadMap)
			if len(freshPlan.selectionOrder) > 0 {
				freshSelectionOrder := s.service.prioritizeOpenAIPromptCacheWarmCandidates(ctx, req, freshPlan.selectionOrder)
				freshResult, freshCompactBlocked, freshAcquireErr := s.tryAcquireOpenAISelectionOrder(ctx, req, freshSelectionOrder)
				if freshAcquireErr != nil {
					return nil, candidateCount, topK, loadSkew, freshAcquireErr
				}
				if freshResult != nil {
					return freshResult, freshPlan.candidateCount, freshPlan.topK, freshPlan.loadSkew, nil
				}
				compactBlocked = compactBlocked || freshCompactBlocked
				selectionOrder = freshSelectionOrder
				candidateCount = freshPlan.candidateCount
				topK = freshPlan.topK
				loadSkew = freshPlan.loadSkew
			}
		}
	}

	cfg := s.service.schedulingConfig()
	// WaitPlan.MaxConcurrency 使用 Concurrency（非 EffectiveLoadFactor），因为 WaitPlan 控制的是 Redis 实际并发槽位等待。
	for _, candidate := range selectionOrder {
		fresh := s.service.resolveFreshSchedulableOpenAIAccount(ctx, candidate.account, req.RequestedModel, false, req.RequiredCapability, req.RequiredImageCapability, req.RequestPlatform)
		if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
			continue
		}
		fresh = s.service.recheckSelectedOpenAIAccountFromDB(ctx, fresh, req.RequestedModel, false, req.RequiredCapability, req.RequiredImageCapability, req.RequestPlatform)
		if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
			continue
		}
		if req.RequireCompact && openAICompactSupportTier(fresh) == 0 {
			compactBlocked = true
			continue
		}
		return &AccountSelectionResult{
			Account: fresh,
			WaitPlan: &AccountWaitPlan{
				AccountID:      fresh.ID,
				MaxConcurrency: fresh.Concurrency,
				Timeout:        cfg.FallbackWaitTimeout,
				MaxWaiting:     cfg.FallbackMaxWaiting,
			},
		}, candidateCount, topK, loadSkew, nil
	}

	return nil, candidateCount, topK, loadSkew, noAvailableOpenAISelectionError(req.RequestedModel, compactBlocked)
}

func (s *defaultOpenAIAccountScheduler) isAccountTransportCompatible(account *Account, requiredTransport OpenAIUpstreamTransport) bool {
	if requiredTransport == OpenAIUpstreamTransportAny || requiredTransport == OpenAIUpstreamTransportHTTPSSE {
		return true
	}
	if s == nil || s.service == nil {
		return false
	}
	return s.service.isOpenAIAccountTransportCompatible(account, requiredTransport)
}

func openAILowestBasePrioritySupportsRequestedModel(baseCandidates []*Account, requestedModel string) bool {
	if len(baseCandidates) == 0 {
		return false
	}
	if requestedModel == "" {
		return true
	}
	minPriority := 0
	found := false
	for _, account := range baseCandidates {
		if account == nil {
			continue
		}
		if !found || account.Priority < minPriority {
			minPriority = account.Priority
			found = true
		}
	}
	if !found {
		return false
	}
	for _, account := range baseCandidates {
		if account != nil && account.Priority == minPriority && account.IsModelSupported(requestedModel) {
			return true
		}
	}
	return false
}

func (s *defaultOpenAIAccountScheduler) shouldIncludeOpenAIAccountInPriorityBase(ctx context.Context, account *Account, requestedModel string) bool {
	if s == nil || s.service == nil || account == nil || !s.service.isOpenAIPoolAccountSoftCooling(account) {
		return true
	}
	if !s.service.isOpenAIPoolAccountSoftCooldownDue(account) {
		return false
	}
	probeModel := requestedModel
	if requestedModel != "" && !account.IsModelSupported(requestedModel) {
		probeModel = ""
	}
	if s.service.clearOpenAIPoolSoftCooldownIfRecoveryProbeDisabled(ctx, account, probeModel) {
		return true
	}
	s.service.maybeStartOpenAIPoolRecoveryProbe(ctx, account, probeModel)
	return false
}

func (s *defaultOpenAIAccountScheduler) isAccountRequestCompatible(ctx context.Context, account *Account, req OpenAIAccountScheduleRequest) bool {
	if account == nil {
		return false
	}
	if !openAIAccountMatchesLockedPriority(account, req.LockedPriority) {
		return false
	}
	if s != nil && s.service != nil && s.service.isOpenAIAccountRuntimeBlocked(account) {
		return false
	}
	if s != nil && s.service != nil && !parentHealthyForShadow(account, s.service.parentAccountLookup(ctx)) {
		return false
	}
	// Quota auto-pause must be evaluated during the initial filter too. Without it the
	// TopK candidate pool can be filled with paused accounts and the later fresh/DB
	// rechecks won't reach healthy accounts that fell outside TopK — manifesting as
	// "no available accounts" even though healthy ones exist.
	if paused, _ := shouldAutoPauseOpenAIAccountByQuota(ctx, account); paused {
		return false
	}
	if req.RequestedModel != "" && !account.IsModelSupported(req.RequestedModel) {
		return false
	}
	if req.GroupID != nil && s != nil && s.service != nil &&
		s.service.needsUpstreamChannelRestrictionCheck(ctx, req.GroupID) &&
		s.service.isUpstreamModelRestrictedByChannel(ctx, *req.GroupID, account, req.RequestedModel, req.RequireCompact) {
		return false
	}
	return accountSupportsOpenAICapabilities(ctx, account, req.RequestedModel, req.RequiredCapability, req.RequiredImageCapability)
}

func (s *defaultOpenAIAccountScheduler) ReportResult(accountID int64, success bool, firstTokenMs *int) {
	s.ReportResultForRequest(accountID, success, firstTokenMs, "")
}

func (s *defaultOpenAIAccountScheduler) ReportResultForRequest(accountID int64, success bool, firstTokenMs *int, requiredImageCapability OpenAIImagesCapability) {
	if s == nil || s.stats == nil {
		return
	}
	s.stats.reportForRequest(accountID, success, firstTokenMs, requiredImageCapability)
}

func (s *defaultOpenAIAccountScheduler) ReportResultForRoute(accountID int64, success bool, firstTokenMs *int, requestedModel string, transport OpenAIUpstreamTransport) {
	if s == nil || s.stats == nil {
		return
	}
	s.stats.reportForRequest(accountID, success, firstTokenMs, "")
	s.stats.reportForRoute(accountID, success, firstTokenMs, requestedModel, transport)
}

func (s *defaultOpenAIAccountScheduler) ReportSwitch() {
	if s == nil {
		return
	}
	s.metrics.recordSwitch()
}

func (s *defaultOpenAIAccountScheduler) SnapshotMetrics() OpenAIAccountSchedulerMetricsSnapshot {
	if s == nil {
		return OpenAIAccountSchedulerMetricsSnapshot{}
	}

	selectTotal := s.metrics.selectTotal.Load()
	prevHit := s.metrics.stickyPreviousHitTotal.Load()
	sessionHit := s.metrics.stickySessionHitTotal.Load()
	switchTotal := s.metrics.accountSwitchTotal.Load()
	latencyTotal := s.metrics.latencyMsTotal.Load()
	loadSkewTotal := s.metrics.loadSkewMilliTotal.Load()

	snapshot := OpenAIAccountSchedulerMetricsSnapshot{
		SelectTotal:              selectTotal,
		StickyPreviousHitTotal:   prevHit,
		StickySessionHitTotal:    sessionHit,
		LoadBalanceSelectTotal:   s.metrics.loadBalanceSelectTotal.Load(),
		AccountSwitchTotal:       switchTotal,
		SchedulerLatencyMsTotal:  latencyTotal,
		RuntimeStatsAccountCount: s.stats.size(),
	}
	if selectTotal > 0 {
		snapshot.SchedulerLatencyMsAvg = float64(latencyTotal) / float64(selectTotal)
		snapshot.StickyHitRatio = float64(prevHit+sessionHit) / float64(selectTotal)
		snapshot.AccountSwitchRate = float64(switchTotal) / float64(selectTotal)
		snapshot.LoadSkewAvg = float64(loadSkewTotal) / 1000 / float64(selectTotal)
	}
	return snapshot
}

func (s *OpenAIGatewayService) openAIAdvancedSchedulerSettingRepo() SettingRepository {
	if s == nil || s.rateLimitService == nil || s.rateLimitService.settingService == nil {
		return nil
	}
	return s.rateLimitService.settingService.settingRepo
}

func (s *OpenAIGatewayService) isOpenAIAdvancedSchedulerEnabled(ctx context.Context) bool {
	if cached, ok := openAIAdvancedSchedulerSettingCache.Load().(*cachedOpenAIAdvancedSchedulerSetting); ok && cached != nil {
		if time.Now().UnixNano() < cached.expiresAt {
			return cached.enabled
		}
	}

	result, _, _ := openAIAdvancedSchedulerSettingSF.Do(openAIAdvancedSchedulerSettingKey, func() (any, error) {
		if cached, ok := openAIAdvancedSchedulerSettingCache.Load().(*cachedOpenAIAdvancedSchedulerSetting); ok && cached != nil {
			if time.Now().UnixNano() < cached.expiresAt {
				return cached.enabled, nil
			}
		}

		enabled := false
		if repo := s.openAIAdvancedSchedulerSettingRepo(); repo != nil {
			dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIAdvancedSchedulerSettingDBTimeout)
			defer cancel()

			value, err := repo.GetValue(dbCtx, openAIAdvancedSchedulerSettingKey)
			if err == nil {
				enabled = strings.EqualFold(strings.TrimSpace(value), "true")
			}
		}

		openAIAdvancedSchedulerSettingCache.Store(&cachedOpenAIAdvancedSchedulerSetting{
			enabled:   enabled,
			expiresAt: time.Now().Add(openAIAdvancedSchedulerSettingCacheTTL).UnixNano(),
		})
		return enabled, nil
	})

	enabled, _ := result.(bool)
	return enabled
}

func (s *OpenAIGatewayService) getOpenAIAccountScheduler(ctx context.Context) OpenAIAccountScheduler {
	if s == nil {
		return nil
	}
	if !s.isOpenAIAdvancedSchedulerEnabled(ctx) {
		return nil
	}
	s.openaiSchedulerOnce.Do(func() {
		if s.openaiScheduler == nil {
			s.openaiScheduler = newDefaultOpenAIAccountScheduler(s, s.getOpenAIAccountRuntimeStats())
		}
	})
	return s.openaiScheduler
}

func (s *OpenAIGatewayService) getOpenAIAccountRuntimeStats() *openAIAccountRuntimeStats {
	if s == nil {
		return nil
	}
	s.openaiAccountStatsOnce.Do(func() {
		if s.openaiAccountStats == nil {
			s.openaiAccountStats = newOpenAIAccountRuntimeStats()
		}
	})
	return s.openaiAccountStats
}

func resetOpenAIAdvancedSchedulerSettingCacheForTest() {
	openAIAdvancedSchedulerSettingCache = atomic.Value{}
	openAIAdvancedSchedulerSettingSF = singleflight.Group{}
}

func (s *OpenAIGatewayService) SelectAccountWithScheduler(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requireCompact bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.selectAccountWithScheduler(ctx, groupID, 0, 0, previousResponseID, sessionHash, 0, requestedModel, excludedIDs, requiredTransport, "", "", requireCompact, PlatformOpenAI, -1)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForCapability(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.SelectAccountWithSchedulerForCapabilityOnPlatform(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requireCompact, PlatformOpenAI)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForCapabilityOnPlatform(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
	requestPlatform string,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requireCompact, requestPlatform, -1)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
	requestPlatform string,
	lockedPriority int,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.selectAccountWithScheduler(ctx, groupID, 0, 0, previousResponseID, sessionHash, 0, requestedModel, excludedIDs, requiredTransport, requiredCapability, "", requireCompact, requestPlatform, lockedPriority)
}

// SelectRequiredAccountForCapabilityOnPlatformLockedPriority revalidates and
// reacquires one exact account. It is used only after a same-account retry has
// already been planned, so the normal priority and health scheduler remains in
// charge of initial selection and account failover.
func (s *OpenAIGatewayService) SelectRequiredAccountForCapabilityOnPlatformLockedPriority(
	ctx context.Context,
	groupID *int64,
	requiredAccountID int64,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
	requestPlatform string,
	lockedPriority int,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.selectRequiredOpenAIAccount(
		ctx,
		groupID,
		requiredAccountID,
		requestedModel,
		excludedIDs,
		requiredTransport,
		requiredCapability,
		"",
		requireCompact,
		requestPlatform,
		lockedPriority,
	)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForCapabilityAndUserSlot(
	ctx context.Context,
	groupID *int64,
	userID int64,
	userConcurrency int,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.SelectAccountWithSchedulerForCapabilityAndUserSlotOnPlatform(ctx, groupID, userID, userConcurrency, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requireCompact, PlatformOpenAI)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForCapabilityAndUserSlotOnPlatform(
	ctx context.Context,
	groupID *int64,
	userID int64,
	userConcurrency int,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
	requestPlatform string,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	decision := OpenAIAccountScheduleDecision{}
	if s == nil || !s.openAIAccountSlotArbiterEnabled() || s.getOpenAIAccountScheduler(ctx) == nil {
		return nil, decision, nil
	}
	return s.selectAccountWithScheduler(ctx, groupID, userID, userConcurrency, previousResponseID, sessionHash, 0, requestedModel, excludedIDs, requiredTransport, requiredCapability, "", requireCompact, requestPlatform, -1)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForCapabilityAndStickyAccount(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	stickyAccountID int64,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.SelectAccountWithSchedulerForCapabilityAndStickyAccountLockedPriority(ctx, groupID, previousResponseID, sessionHash, stickyAccountID, requestedModel, excludedIDs, requiredTransport, requiredCapability, requireCompact, -1)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForCapabilityAndStickyAccountLockedPriority(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	stickyAccountID int64,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
	lockedPriority int,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.selectAccountWithScheduler(ctx, groupID, 0, 0, previousResponseID, sessionHash, stickyAccountID, requestedModel, excludedIDs, requiredTransport, requiredCapability, "", requireCompact, PlatformOpenAI, lockedPriority)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForImages(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredCapability OpenAIImagesCapability,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.SelectAccountWithSchedulerForImagesLockedPriority(ctx, groupID, sessionHash, requestedModel, excludedIDs, requiredCapability, -1)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForImagesLockedPriority(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredCapability OpenAIImagesCapability,
	lockedPriority int,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	selection, decision, err := s.selectAccountWithScheduler(ctx, groupID, 0, 0, "", sessionHash, 0, requestedModel, excludedIDs, OpenAIUpstreamTransportHTTPSSE, "", requiredCapability, false, PlatformOpenAI, lockedPriority)
	if err == nil && selection != nil && selection.Account != nil {
		return selection, decision, nil
	}
	// 如果要求 native 能力（如指定了模型）但没有可用的 APIKey 账号，回退到 basic（OAuth 账号）
	if requiredCapability == OpenAIImagesCapabilityNative {
		return s.selectAccountWithScheduler(ctx, groupID, 0, 0, "", sessionHash, 0, requestedModel, excludedIDs, OpenAIUpstreamTransportHTTPSSE, "", OpenAIImagesCapabilityBasic, false, PlatformOpenAI, lockedPriority)
	}
	return selection, decision, err
}

func (s *OpenAIGatewayService) SelectRequiredAccountForImagesLockedPriority(
	ctx context.Context,
	groupID *int64,
	requiredAccountID int64,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredCapability OpenAIImagesCapability,
	lockedPriority int,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.selectRequiredOpenAIAccount(
		ctx,
		groupID,
		requiredAccountID,
		requestedModel,
		excludedIDs,
		OpenAIUpstreamTransportHTTPSSE,
		"",
		requiredCapability,
		false,
		PlatformOpenAI,
		lockedPriority,
	)
}

func (s *OpenAIGatewayService) selectRequiredOpenAIAccount(
	ctx context.Context,
	groupID *int64,
	requiredAccountID int64,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requireCompact bool,
	requestPlatform string,
	lockedPriority int,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	decision := OpenAIAccountScheduleDecision{Layer: openAIAccountScheduleLayerSessionSticky}
	if s == nil || requiredAccountID <= 0 {
		return nil, decision, ErrNoAvailableAccounts
	}
	if _, excluded := excludedIDs[requiredAccountID]; excluded {
		return nil, decision, ErrNoAvailableAccounts
	}

	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		return nil, decision, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}

	account, err := s.getSchedulableAccount(ctx, requiredAccountID)
	if err != nil || account == nil {
		return nil, decision, ErrNoAvailableAccounts
	}
	if !isOpenAICompatibleRequestPlatformAccount(account, requestPlatform) ||
		!account.IsSchedulable() ||
		!openAIAccountMatchesLockedPriority(account, lockedPriority) ||
		!s.isOpenAIAccountTransportCompatible(account, requiredTransport) ||
		!accountSupportsOpenAICapabilities(ctx, account, requestedModel, requiredCapability, requiredImageCapability) ||
		s.isOpenAIAccountRuntimeBlocked(account) ||
		!s.latestOpenAIAccountMatchesGroup(ctx, account, groupID) {
		return nil, decision, ErrNoAvailableAccounts
	}
	if !parentHealthyForShadow(account, s.parentAccountLookup(ctx)) {
		return nil, decision, ErrNoAvailableAccounts
	}
	if s.needsUpstreamChannelRestrictionCheck(ctx, groupID) &&
		s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, requestedModel, requireCompact) {
		return nil, decision, ErrNoAvailableAccounts
	}

	account = s.recheckSelectedOpenAIAccountFromDB(
		ctx,
		account,
		requestedModel,
		requireCompact,
		requiredCapability,
		requiredImageCapability,
		requestPlatform,
	)
	if account == nil || !s.isOpenAIAccountTransportCompatible(account, requiredTransport) ||
		!openAIAccountMatchesLockedPriority(account, lockedPriority) ||
		!s.latestOpenAIAccountMatchesGroup(ctx, account, groupID) {
		return nil, decision, ErrNoAvailableAccounts
	}

	result, err := s.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
	if err != nil {
		return nil, decision, err
	}
	decision.StickySessionHit = true
	decision.SelectedAccountID = account.ID
	decision.SelectedAccountType = account.Type
	if result != nil && result.Acquired {
		selection, selectErr := s.newAcquiredSelectionResult(ctx, account, result.ReleaseFunc)
		return selection, decision, selectErr
	}

	cfg := s.schedulingConfig()
	selection, selectErr := s.newSelectionResult(ctx, account, false, nil, &AccountWaitPlan{
		AccountID:      account.ID,
		MaxConcurrency: account.Concurrency,
		Timeout:        cfg.FallbackWaitTimeout,
		MaxWaiting:     cfg.FallbackMaxWaiting,
	})
	return selection, decision, selectErr
}

func (s *OpenAIGatewayService) selectAccountWithScheduler(
	ctx context.Context,
	groupID *int64,
	userID int64,
	userConcurrency int,
	previousResponseID string,
	sessionHash string,
	stickyAccountID int64,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requireCompact bool,
	requestPlatform string,
	lockedPriority int,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	decision := OpenAIAccountScheduleDecision{}
	scheduler := s.getOpenAIAccountScheduler(ctx)
	if scheduler == nil {
		decision.Layer = openAIAccountScheduleLayerLoadBalance
		if requiredTransport == OpenAIUpstreamTransportAny || requiredTransport == OpenAIUpstreamTransportHTTPSSE {
			effectiveExcludedIDs := cloneExcludedAccountIDs(excludedIDs)
			for {
				selection, err := s.selectAccountWithLoadAwareness(ctx, groupID, sessionHash, requestedModel, effectiveExcludedIDs, requireCompact, stickyAccountID, requiredCapability, requiredImageCapability, requestPlatform, lockedPriority)
				if err != nil {
					return nil, decision, err
				}
				if selection == nil || selection.Account == nil {
					return selection, decision, nil
				}
				if accountSupportsOpenAICapabilities(ctx, selection.Account, requestedModel, requiredCapability, requiredImageCapability) {
					return selection, decision, nil
				}
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				if effectiveExcludedIDs == nil {
					effectiveExcludedIDs = make(map[int64]struct{})
				}
				if _, exists := effectiveExcludedIDs[selection.Account.ID]; exists {
					return nil, decision, ErrNoAvailableAccounts
				}
				effectiveExcludedIDs[selection.Account.ID] = struct{}{}
			}
		}

		effectiveExcludedIDs := cloneExcludedAccountIDs(excludedIDs)
		for {
			selection, err := s.selectAccountWithLoadAwareness(ctx, groupID, sessionHash, requestedModel, effectiveExcludedIDs, requireCompact, stickyAccountID, requiredCapability, requiredImageCapability, requestPlatform, lockedPriority)
			if err != nil {
				return nil, decision, err
			}
			if selection == nil || selection.Account == nil {
				return selection, decision, nil
			}
			if s.isOpenAIAccountTransportCompatible(selection.Account, requiredTransport) &&
				accountSupportsOpenAICapabilities(ctx, selection.Account, requestedModel, requiredCapability, requiredImageCapability) {
				return selection, decision, nil
			}
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			if effectiveExcludedIDs == nil {
				effectiveExcludedIDs = make(map[int64]struct{})
			}
			if _, exists := effectiveExcludedIDs[selection.Account.ID]; exists {
				return nil, decision, ErrNoAvailableAccounts
			}
			effectiveExcludedIDs[selection.Account.ID] = struct{}{}
		}
	}

	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		slog.Warn("channel pricing restriction blocked request",
			"group_id", derefGroupID(groupID),
			"model", requestedModel)
		return nil, decision, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}

	if stickyAccountID <= 0 && sessionHash != "" && s.cache != nil {
		if accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash); err == nil && accountID > 0 {
			stickyAccountID = accountID
		}
	}
	return scheduler.Select(ctx, OpenAIAccountScheduleRequest{
		GroupID:                 groupID,
		UserID:                  userID,
		UserConcurrency:         userConcurrency,
		SessionHash:             sessionHash,
		StickyAccountID:         stickyAccountID,
		PreviousResponseID:      previousResponseID,
		RequestedModel:          requestedModel,
		RequiredTransport:       requiredTransport,
		RequiredCapability:      requiredCapability,
		RequiredImageCapability: requiredImageCapability,
		RequireCompact:          requireCompact,
		RequestPlatform:         requestPlatform,
		ExcludedIDs:             excludedIDs,
		LockedPriority:          lockedPriority,
	})
}

func accountSupportsOpenAICapabilities(ctx context.Context, account *Account, requestedModel string, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability) bool {
	if account == nil {
		return false
	}
	return account.SupportsOpenAIEndpointCapability(requiredCapability) &&
		account.SupportsOpenAIImageCapability(requiredImageCapability) &&
		account.MatchesOpenAIImagePoolRequest(ctx, requestedModel, requiredImageCapability)
}

func cloneExcludedAccountIDs(excludedIDs map[int64]struct{}) map[int64]struct{} {
	if len(excludedIDs) == 0 {
		return nil
	}
	cloned := make(map[int64]struct{}, len(excludedIDs))
	for id := range excludedIDs {
		cloned[id] = struct{}{}
	}
	return cloned
}

func (s *OpenAIGatewayService) isOpenAIAccountTransportCompatible(account *Account, requiredTransport OpenAIUpstreamTransport) bool {
	if requiredTransport == OpenAIUpstreamTransportAny || requiredTransport == OpenAIUpstreamTransportHTTPSSE {
		return true
	}
	if s == nil || account == nil {
		return false
	}
	if requiredTransport == OpenAIUpstreamTransportResponsesWebsocketV2Ingress {
		if s.cfg == nil || !s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled {
			return s.getOpenAIWSProtocolResolver().Resolve(account).Transport == OpenAIUpstreamTransportResponsesWebsocketV2
		}
		mode := account.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault)
		if mode == OpenAIWSIngressModeHTTPBridge {
			return true
		}
		return s.getOpenAIWSProtocolResolver().Resolve(account).Transport == OpenAIUpstreamTransportResponsesWebsocketV2
	}
	return s.getOpenAIWSProtocolResolver().Resolve(account).Transport == requiredTransport
}

func (s *OpenAIGatewayService) ReportOpenAIAccountScheduleResult(accountID int64, success bool, firstTokenMs *int) {
	s.reportOpenAIAccountScheduleResultForCapability(accountID, success, firstTokenMs, "")
}

func (s *OpenAIGatewayService) ReportOpenAIAccountScheduleResultForRequest(account *Account, requestedModel string, success bool, firstTokenMs *int) {
	if s == nil || account == nil {
		return
	}
	if success {
		s.openaiPoolSoftCooldownUntil.Delete(account.ID)
		s.openaiPoolSoftCooldownFailureCount.Delete(account.ID)
	}
	stats := s.getOpenAIAccountRuntimeStats()
	if stats == nil {
		return
	}
	transport := s.getOpenAIWSProtocolResolver().Resolve(account).Transport
	stats.reportForRequest(account.ID, success, firstTokenMs, "")
	stats.reportForRoute(account.ID, success, firstTokenMs, requestedModel, transport)
}

func (s *OpenAIGatewayService) ReportOpenAIImageAccountScheduleResult(accountID int64, success bool, firstTokenMs *int, requiredCapability OpenAIImagesCapability) {
	if requiredCapability == "" {
		requiredCapability = OpenAIImagesCapabilityBasic
	}
	s.reportOpenAIAccountScheduleResultForCapability(accountID, success, firstTokenMs, requiredCapability)
}

func (s *OpenAIGatewayService) reportOpenAIAccountScheduleResultForCapability(accountID int64, success bool, firstTokenMs *int, requiredImageCapability OpenAIImagesCapability) {
	if s == nil {
		return
	}
	if success {
		s.openaiPoolSoftCooldownUntil.Delete(accountID)
		s.openaiPoolSoftCooldownFailureCount.Delete(accountID)
	}
	stats := s.getOpenAIAccountRuntimeStats()
	if stats == nil {
		return
	}
	stats.reportForRequest(accountID, success, firstTokenMs, requiredImageCapability)
}

func (s *OpenAIGatewayService) RecordOpenAIAccountSwitch() {
	scheduler := s.getOpenAIAccountScheduler(context.Background())
	if scheduler == nil {
		return
	}
	scheduler.ReportSwitch()
}

func (s *OpenAIGatewayService) SnapshotOpenAIAccountSchedulerMetrics() OpenAIAccountSchedulerMetricsSnapshot {
	scheduler := s.getOpenAIAccountScheduler(context.Background())
	if scheduler == nil {
		return OpenAIAccountSchedulerMetricsSnapshot{}
	}
	return scheduler.SnapshotMetrics()
}

func (s *OpenAIGatewayService) openAIWSSessionStickyTTL() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds > 0 {
		return time.Duration(s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds) * time.Second
	}
	return openaiStickySessionTTL
}

func (s *OpenAIGatewayService) openAIWSLBTopK() int {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.LBTopK > 0 {
		return s.cfg.Gateway.OpenAIWS.LBTopK
	}
	return 7
}

func (s *OpenAIGatewayService) openAIWSSchedulerWeights() GatewayOpenAIWSSchedulerScoreWeightsView {
	if s != nil && s.cfg != nil {
		return GatewayOpenAIWSSchedulerScoreWeightsView{
			Priority:      s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority,
			Load:          s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load,
			Queue:         s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue,
			ErrorRate:     s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate,
			TTFT:          s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT,
			Reset:         s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Reset,
			QuotaHeadroom: s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.QuotaHeadroom,
		}
	}
	return GatewayOpenAIWSSchedulerScoreWeightsView{
		Priority:      1.0,
		Load:          1.0,
		Queue:         0.7,
		ErrorRate:     0.8,
		TTFT:          0.5,
		Reset:         0.0,
		QuotaHeadroom: 0.0,
	}
}

type GatewayOpenAIWSSchedulerScoreWeightsView struct {
	Priority      float64
	Load          float64
	Queue         float64
	ErrorRate     float64
	TTFT          float64
	Reset         float64
	QuotaHeadroom float64
}

func openAIQuotaHeadroomFactor(account *Account, now time.Time) float64 {
	if account == nil || len(account.Extra) == 0 || openAIQuotaHeadroomSnapshotStale(account.Extra, now) {
		return openAIQuotaHeadroomNeutralFactor
	}
	primaryUsedPercent, ok := resolveAccountExtraNumber(account.Extra, "codex_primary_used_percent", "codex_7d_used_percent")
	if !ok || openAIQuotaWindowResetAny(account.Extra, now, "primary", "7d") {
		return openAIQuotaHeadroomNeutralFactor
	}

	factor := 1 - clamp01(primaryUsedPercent/100)
	if secondaryUsedPercent, ok := resolveAccountExtraNumber(account.Extra, "codex_secondary_used_percent", "codex_5h_used_percent"); ok &&
		!openAIQuotaWindowResetAny(account.Extra, now, "secondary", "5h") {
		secondaryRemaining := 1 - clamp01(secondaryUsedPercent/100)
		if secondaryRemaining < openAIQuotaHeadroomSecondaryLowRemain {
			factor *= openAIQuotaHeadroomNeutralFactor
		}
	}
	return factor
}

func openAIQuotaHeadroomSnapshotStale(extra map[string]any, now time.Time) bool {
	updatedRaw, ok := extra["codex_usage_updated_at"]
	if !ok {
		return true
	}
	updatedAt, err := parseTime(fmt.Sprint(updatedRaw))
	if err != nil {
		return true
	}
	return now.Sub(updatedAt) >= openAIQuotaHeadroomSnapshotStaleAfter
}

func openAIQuotaWindowResetAny(extra map[string]any, now time.Time, windows ...string) bool {
	for _, window := range windows {
		if openAIQuotaWindowReset(extra, window, now) {
			return true
		}
	}
	return false
}

func clamp01(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

func calcLoadSkewByMoments(sum float64, sumSquares float64, count int) float64 {
	if count <= 1 {
		return 0
	}
	mean := sum / float64(count)
	variance := sumSquares/float64(count) - mean*mean
	if variance < 0 {
		variance = 0
	}
	return math.Sqrt(variance)
}
