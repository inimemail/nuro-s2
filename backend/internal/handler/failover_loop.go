package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/runtimeops"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

// TempUnscheduler 用于 HandleFailoverError 中同账号重试耗尽后的临时封禁。
// GatewayService 隐式实现此接口。
type TempUnscheduler interface {
	TempUnscheduleRetryableError(ctx context.Context, accountID int64, failoverErr *service.UpstreamFailoverError)
}

// FailoverAction 表示 failover 错误处理后的下一步动作
type FailoverAction int

const (
	// FailoverContinue 继续循环（同账号重试或切换账号，调用方统一 continue）
	FailoverContinue FailoverAction = iota
	// FailoverExhausted 切换次数耗尽（调用方应返回错误响应）
	FailoverExhausted
	// FailoverCanceled context 已取消（调用方应直接 return）
	FailoverCanceled
)

const (
	// maxSameAccountRetries 同账号重试次数上限（针对 RetryableOnSameAccount 错误）
	maxSameAccountRetries = 3
	// sameAccountRetryDelay 同账号重试间隔
	sameAccountRetryDelay = 500 * time.Millisecond
	// singleAccountBackoffDelay 单账号分组 503 退避重试固定延时。
	// Service 层在 SingleAccountRetry 模式下已做充分原地重试（最多 3 次、总等待 30s），
	// Handler 层只需短暂间隔后重新进入 Service 层即可。
	singleAccountBackoffDelay = 2 * time.Second

	// These keys live in the existing retry-start map. They are deliberately
	// outside the positive account-ID range and let race-enabled requests share
	// one immutable deadline without changing every handler's local state type.
	sharedRaceRetryDeadlineKey  int64 = 0
	sharedRaceRetryStartedKey   int64 = -1
	sharedRaceRetryExhaustedKey int64 = -2
)

func sameAccountRetryDelayForAccount(account *service.Account) time.Duration {
	if account == nil {
		return sameAccountRetryDelay
	}
	return account.GetPoolModeSameAccountRetryDelay()
}

type sameAccountRetryPlan struct {
	RetryLimit int
	RetryCount int
	RuleKey    string
	RuleLimit  int
	RuleCount  int
	Delay      time.Duration
	Elapsed    time.Duration
	MaxElapsed time.Duration
}

// sameAccountRetryRuleCounts keeps status/transport sub-limits separate from
// the request-level per-account total. It is request-local and bounded by the
// number of accounts touched by the request.
type sameAccountRetryRuleCounts map[int64]map[string]int

func resolveSameAccountRaceRetryRule(account *service.Account, failoverErr *service.UpstreamFailoverError) (key string, limit int, matched bool) {
	if account == nil || failoverErr == nil {
		return "", 0, false
	}
	statusCode := failoverErr.StatusCode
	// Transport failures may be exposed as a synthetic 502. Only an explicit
	// transport marker may select the transport rule; a real HTTP 502 must use
	// the configured exact 502 rule.
	if failoverErr.RetryRuleTransport && (statusCode == 0 || statusCode == http.StatusBadGateway) {
		statusCode = 0
	} else if failoverErr.RetryRuleTransport {
		// A transport marker from a previous synthetic error must not survive
		// onto a real HTTP response status.
		failoverErr.RetryRuleTransport = false
	}
	return account.OpenAIUpstreamConcurrencyRaceRetryRule(statusCode)
}

func markSameAccountAttemptStart(starts map[int64]time.Time, account *service.Account, startedAt time.Time) {
	if starts == nil || account == nil || account.ID == 0 || startedAt.IsZero() {
		return
	}
	// Race-enabled OpenAI requests start their budget when the first eligible
	// upstream error is classified, not when the initial upstream attempt starts.
	if account.IsOpenAIUpstreamConcurrencyRaceEnabled() {
		return
	}
	if _, ok := starts[account.ID]; !ok {
		starts[account.ID] = startedAt
	}
}

// initialSameAccountRetryStart keeps the legacy per-account start timestamp
// while allowing race mode to start its shared deadline at the first eligible
// upstream failure instead of charging request preparation time.
func initialSameAccountRetryStart(account *service.Account, startedAt time.Time) map[int64]time.Time {
	starts := make(map[int64]time.Time)
	if account != nil && account.ID != 0 && !account.IsOpenAIUpstreamConcurrencyRaceEnabled() && !startedAt.IsZero() {
		starts[account.ID] = startedAt
	}
	return starts
}

func activeSharedRaceDeadline(starts map[int64]time.Time) (time.Time, bool) {
	if len(starts) == 0 || !starts[sharedRaceRetryExhaustedKey].IsZero() {
		return time.Time{}, false
	}
	deadline := starts[sharedRaceRetryDeadlineKey]
	if deadline.IsZero() {
		return time.Time{}, false
	}
	if !deadline.After(time.Now()) {
		starts[sharedRaceRetryExhaustedKey] = time.Now()
		runtimeops.ObservePreemptionExhausted()
		return time.Time{}, false
	}
	return deadline, true
}

func sharedRaceResponseHeaderContext(ctx context.Context, starts map[int64]time.Time) context.Context {
	deadline, ok := activeSharedRaceDeadline(starts)
	if !ok {
		return ctx
	}
	return service.WithHTTPUpstreamResponseHeaderDeadline(ctx, deadline)
}

func planSameAccountRetry(account *service.Account, counts map[int64]int, starts map[int64]time.Time, delay time.Duration, failoverErr ...*service.UpstreamFailoverError) (sameAccountRetryPlan, bool) {
	return planSameAccountRetryWithRuleCounts(account, counts, nil, starts, delay, 0, failoverErr...)
}

func planSameAccountRetryWithMaxElapsed(account *service.Account, counts map[int64]int, starts map[int64]time.Time, delay, maxElapsed time.Duration, failoverErr ...*service.UpstreamFailoverError) (sameAccountRetryPlan, bool) {
	return planSameAccountRetryWithRuleCounts(account, counts, nil, starts, delay, maxElapsed, failoverErr...)
}

func planSameAccountRetryWithRuleCounts(account *service.Account, counts map[int64]int, ruleCounts sameAccountRetryRuleCounts, starts map[int64]time.Time, delay, maxElapsed time.Duration, failoverErr ...*service.UpstreamFailoverError) (sameAccountRetryPlan, bool) {
	plan := sameAccountRetryPlan{Delay: delay}
	if account == nil || counts == nil {
		return plan, false
	}
	if starts == nil {
		starts = make(map[int64]time.Time)
	}
	accountID := account.ID
	plan.RetryLimit = account.GetPoolModeRetryCount()
	if counts[accountID] >= plan.RetryLimit {
		return plan, false
	}
	if account.IsOpenAIUpstreamConcurrencyRaceEnabled() {
		// Real race callers provide the request-local rule map and the current
		// upstream error. Keep the nil-map wrapper compatible for non-OpenAI
		// legacy callers and unit helpers; it is never used by production OpenAI
		// race paths and therefore cannot bypass their configured sub-limits.
		if len(failoverErr) == 0 || failoverErr[0] == nil {
			if ruleCounts != nil {
				return plan, false
			}
		} else {
			var matched bool
			plan.RuleKey, plan.RuleLimit, matched = resolveSameAccountRaceRetryRule(account, failoverErr[0])
			if !matched || plan.RuleKey == "" {
				return plan, false
			}
		}
		if plan.RuleKey != "" && plan.RuleLimit >= 0 {
			if ruleCounts == nil {
				// Legacy callers without a rule map retain the total limit, but
				// never receive a larger limit than the resolved rule.
				if counts[accountID] >= plan.RuleLimit {
					return plan, false
				}
			} else {
				byRule := ruleCounts[accountID]
				// A restored Edge continuation from an older binary may contain
				// only the aggregate retry count. Starting fresh per-rule counters
				// would let an already-consumed exact-status allowance run again.
				// Fail closed for that account while preserving normal accounts and
				// new continuations, which always persist rule counters.
				if counts[accountID] > 0 && len(byRule) == 0 {
					return plan, false
				}
				plan.RuleCount = byRule[plan.RuleKey]
				if plan.RuleCount >= plan.RuleLimit {
					return plan, false
				}
			}
		}
	}
	plan.MaxElapsed = account.GetPoolModeSameAccountRetryMaxElapsed()
	if maxElapsed > 0 && plan.MaxElapsed > maxElapsed {
		plan.MaxElapsed = maxElapsed
	}
	sharedDeadline, hasSharedDeadline := starts[sharedRaceRetryDeadlineKey]
	sharedBudget := hasSharedDeadline && !sharedDeadline.IsZero()
	if sharedBudget || (account.IsOpenAIUpstreamConcurrencyRaceEnabled() && plan.MaxElapsed > 0) {
		now := time.Now()
		if !sharedBudget {
			// The first account that enters the race owns the request budget. Store
			// the deadline separately so a later account cannot extend it with its
			// own account-level setting.
			startedAt := starts[accountID]
			if startedAt.IsZero() {
				startedAt = now
			}
			sharedDeadline = startedAt.Add(plan.MaxElapsed)
			starts[sharedRaceRetryStartedKey] = startedAt
			starts[sharedRaceRetryDeadlineKey] = sharedDeadline
			runtimeops.ObservePreemptionStarted()
			// Keep the legacy account entry for diagnostics and callers that
			// inspect the old map; it is not used as the race budget source.
			if accountID > 0 {
				starts[accountID] = startedAt
			}
			sharedBudget = true
		} else if starts[sharedRaceRetryStartedKey].IsZero() {
			// Continuations from an older Edge binary may carry only a deadline.
			if plan.MaxElapsed > 0 {
				starts[sharedRaceRetryStartedKey] = sharedDeadline.Add(-plan.MaxElapsed)
			} else {
				starts[sharedRaceRetryStartedKey] = now
			}
		}
		if !starts[sharedRaceRetryExhaustedKey].IsZero() {
			return plan, false
		}
		startedAt := starts[sharedRaceRetryStartedKey]
		plan.Elapsed = now.Sub(startedAt)
		if plan.Elapsed < 0 {
			plan.Elapsed = 0
		}
		remaining := sharedDeadline.Sub(now)
		if remaining <= 0 {
			if starts[sharedRaceRetryExhaustedKey].IsZero() {
				starts[sharedRaceRetryExhaustedKey] = now
				runtimeops.ObservePreemptionExhausted()
			}
			return plan, false
		}
		if delay >= remaining {
			// The configured delay does not fit in the remaining window, but an
			// immediate retry or a switched account may still use that window.
			return plan, false
		}
	} else if plan.MaxElapsed > 0 {
		// Legacy per-account elapsed cap for accounts that are not participating
		// in a request-level OpenAI race.
		now := time.Now()
		startedAt, ok := starts[accountID]
		if !ok || startedAt.IsZero() {
			startedAt = now
			if starts != nil {
				starts[accountID] = startedAt
			}
		}
		plan.Elapsed = now.Sub(startedAt)
		if plan.Elapsed < 0 {
			plan.Elapsed = 0
		}
		remaining := plan.MaxElapsed - plan.Elapsed
		if remaining <= 0 || delay >= remaining {
			return plan, false
		}
	}
	counts[accountID]++
	plan.RetryCount = counts[accountID]
	if plan.RuleKey != "" && ruleCounts != nil {
		if ruleCounts[accountID] == nil {
			ruleCounts[accountID] = make(map[string]int)
		}
		ruleCounts[accountID][plan.RuleKey]++
		plan.RuleCount = ruleCounts[accountID][plan.RuleKey]
	}
	return plan, true
}

// FailoverState 跨循环迭代共享的 failover 状态
type FailoverState struct {
	SwitchCount               int
	MaxSwitches               int
	FailedAccountIDs          map[int64]struct{}
	SameAccountRetryCount     map[int64]int
	SameAccountRetryRuleCount sameAccountRetryRuleCounts
	SameAccountRetryStart     map[int64]time.Time
	LastFailoverErr           *service.UpstreamFailoverError
	ForceCacheBilling         bool
	hasBoundSession           bool
	pendingRetryAccountID     int64
	pendingRetryPlatform      string
	pendingRetryPoolMode      bool
	pendingRetrySharedRace    bool
	pendingRetryErr           *service.UpstreamFailoverError
}

// NewFailoverState 创建 failover 状态
func NewFailoverState(maxSwitches int, hasBoundSession bool) *FailoverState {
	return &FailoverState{
		MaxSwitches:               maxSwitches,
		FailedAccountIDs:          make(map[int64]struct{}),
		SameAccountRetryCount:     make(map[int64]int),
		SameAccountRetryRuleCount: make(sameAccountRetryRuleCounts),
		SameAccountRetryStart:     make(map[int64]time.Time),
		hasBoundSession:           hasBoundSession,
	}
}

// HandleFailoverError 处理 UpstreamFailoverError，返回下一步动作。
// 包含：缓存计费判断、同账号重试、临时封禁、切换计数、Antigravity 延时。
func (s *FailoverState) HandleFailoverError(
	ctx context.Context,
	gatewayService TempUnscheduler,
	accountID int64,
	platform string,
	failoverErr *service.UpstreamFailoverError,
) FailoverAction {
	return s.HandleFailoverErrorWithRetryLimit(ctx, gatewayService, accountID, platform, maxSameAccountRetries, failoverErr)
}

func (s *FailoverState) HandleFailoverErrorWithRetryLimit(
	ctx context.Context,
	gatewayService TempUnscheduler,
	accountID int64,
	platform string,
	retryLimit int,
	failoverErr *service.UpstreamFailoverError,
) FailoverAction {
	return s.handleFailoverErrorWithRetryPlan(ctx, gatewayService, accountID, platform, retryLimit, sameAccountRetryDelay, 0, false, failoverErr)
}

// HandleFailoverErrorForAccount applies the account-level retry count, delay,
// and elapsed-time budget used by the custom pool/race scheduler. Keep the
// legacy wrappers above for callers and tests that intentionally use the
// historical fixed 500ms policy.
func (s *FailoverState) HandleFailoverErrorForAccount(
	ctx context.Context,
	gatewayService TempUnscheduler,
	account *service.Account,
	failoverErr *service.UpstreamFailoverError,
) FailoverAction {
	if account == nil {
		return s.HandleFailoverError(ctx, gatewayService, 0, "", failoverErr)
	}
	// The account-level retry settings are pool-mode controls. Keep the
	// historical three-attempt behavior for non-pool accounts, whose current
	// GetPoolModeRetryCount fallback is intentionally one for the custom pool
	// scheduler.
	retryLimit := maxSameAccountRetries
	retryDelay := sameAccountRetryDelay
	maxElapsed := time.Duration(0)
	if account.IsPoolMode() {
		retryLimit = account.GetPoolModeRetryCount()
		retryDelay = sameAccountRetryDelayForAccount(account)
		maxElapsed = account.GetPoolModeSameAccountRetryMaxElapsed()
	}
	if account.IsOpenAIUpstreamConcurrencyRaceEnabled() && failoverErr != nil && failoverErr.RetryableOnSameAccount {
		var matched bool
		failoverErr.RetryRuleKey, failoverErr.RetryRuleLimit, matched = resolveSameAccountRaceRetryRule(account, failoverErr)
		failoverErr.RetryableOnSameAccount = matched && failoverErr.RetryRuleKey != "" && failoverErr.RetryRuleLimit > 0
	}
	sharedRaceBudget := account.IsOpenAIUpstreamConcurrencyRaceEnabled()
	if s != nil && s.SameAccountRetryStart != nil && !s.SameAccountRetryStart[sharedRaceRetryDeadlineKey].IsZero() {
		sharedRaceBudget = true
	}
	return s.handleFailoverErrorWithRetryPlanAndBudget(
		ctx,
		gatewayService,
		account.ID,
		account.Platform,
		retryLimit,
		retryDelay,
		maxElapsed,
		account.IsPoolMode(),
		sharedRaceBudget,
		failoverErr,
	)
}

func (s *FailoverState) handleFailoverErrorWithRetryPlan(
	ctx context.Context,
	gatewayService TempUnscheduler,
	accountID int64,
	platform string,
	retryLimit int,
	retryDelay time.Duration,
	retryMaxElapsed time.Duration,
	poolMode bool,
	failoverErr *service.UpstreamFailoverError,
) FailoverAction {
	return s.handleFailoverErrorWithRetryPlanAndBudget(ctx, gatewayService, accountID, platform, retryLimit, retryDelay, retryMaxElapsed, poolMode, false, failoverErr)
}

func (s *FailoverState) handleFailoverErrorWithRetryPlanAndBudget(
	ctx context.Context,
	gatewayService TempUnscheduler,
	accountID int64,
	platform string,
	retryLimit int,
	retryDelay time.Duration,
	retryMaxElapsed time.Duration,
	poolMode bool,
	sharedRaceBudget bool,
	failoverErr *service.UpstreamFailoverError,
) FailoverAction {
	if ctx != nil && ctx.Err() != nil {
		return FailoverCanceled
	}
	s.clearPendingSameAccountRetry()
	s.LastFailoverErr = failoverErr

	// 同账号重试不算切换账号，粘性会话只在实际切号时强制缓存计费。
	sameAccountRetry := failoverErr.RetryableOnSameAccount &&
		s.sameAccountRetryAllowedWithBudget(accountID, retryLimit, retryDelay, retryMaxElapsed, sharedRaceBudget, failoverErr)
	if needForceCacheBilling(s.hasBoundSession, failoverErr, sameAccountRetry) {
		s.ForceCacheBilling = true
	}

	// 同账号重试：对 RetryableOnSameAccount 的临时性错误，先在同一账号上重试
	if sameAccountRetry {
		s.SameAccountRetryCount[accountID]++
		if failoverErr.RetryRuleKey != "" {
			if s.SameAccountRetryRuleCount[accountID] == nil {
				s.SameAccountRetryRuleCount[accountID] = make(map[string]int)
			}
			s.SameAccountRetryRuleCount[accountID][failoverErr.RetryRuleKey]++
		}
		s.pendingRetryAccountID = accountID
		s.pendingRetryPlatform = platform
		s.pendingRetryPoolMode = poolMode
		s.pendingRetrySharedRace = sharedRaceBudget
		s.pendingRetryErr = failoverErr
		logger.FromContext(ctx).Warn("gateway.failover_same_account_retry",
			zap.Int64("account_id", accountID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("same_account_retry_count", s.SameAccountRetryCount[accountID]),
			zap.Int("same_account_retry_max", retryLimit),
			zap.String("retry_rule", failoverErr.RetryRuleKey),
			zap.Int("retry_rule_limit", failoverErr.RetryRuleLimit),
		)
		if !sleepWithContext(ctx, retryDelay) {
			return FailoverCanceled
		}
		return FailoverContinue
	}

	// 同账号重试用尽，执行临时封禁
	if failoverErr.RetryableOnSameAccount && !poolMode {
		gatewayService.TempUnscheduleRetryableError(ctx, accountID, failoverErr)
	}

	// 加入失败列表
	s.FailedAccountIDs[accountID] = struct{}{}

	// 检查是否耗尽
	if s.SwitchCount >= s.MaxSwitches {
		return FailoverExhausted
	}

	// 递增切换计数
	s.SwitchCount++
	logger.FromContext(ctx).Warn("gateway.failover_switch_account",
		zap.Int64("account_id", accountID),
		zap.Int("upstream_status", failoverErr.StatusCode),
		zap.Int("switch_count", s.SwitchCount),
		zap.Int("max_switches", s.MaxSwitches),
	)

	// Antigravity 平台换号线性递增延时
	if platform == service.PlatformAntigravity {
		delay := time.Duration(s.SwitchCount-1) * time.Second
		if !sleepWithContext(ctx, delay) {
			return FailoverCanceled
		}
	}

	return FailoverContinue
}

func (s *FailoverState) pendingSameAccountRetryID() int64 {
	if s == nil {
		return 0
	}
	return s.pendingRetryAccountID
}

func (s *FailoverState) clearPendingSameAccountRetry() {
	if s == nil {
		return
	}
	s.pendingRetryAccountID = 0
	s.pendingRetryPlatform = ""
	s.pendingRetryPoolMode = false
	s.pendingRetrySharedRace = false
	s.pendingRetryErr = nil
}

// settleUnavailableSameAccountRetry records the original upstream failure when
// the exact account becomes unavailable before the planned retry can start.
func (s *FailoverState) settleUnavailableSameAccountRetry(ctx context.Context, gatewayService TempUnscheduler) FailoverAction {
	if s == nil || s.pendingRetryAccountID <= 0 || s.pendingRetryErr == nil {
		return FailoverContinue
	}
	accountID := s.pendingRetryAccountID
	platform := s.pendingRetryPlatform
	poolMode := s.pendingRetryPoolMode
	sharedRaceBudget := s.pendingRetrySharedRace
	failoverErr := s.pendingRetryErr
	s.clearPendingSameAccountRetry()
	return s.handleFailoverErrorWithRetryPlanAndBudget(ctx, gatewayService, accountID, platform, 0, 0, 0, poolMode, sharedRaceBudget, failoverErr)
}

func (s *FailoverState) sameAccountRetryAllowed(accountID int64, retryLimit int, retryDelay, maxElapsed time.Duration) bool {
	return s.sameAccountRetryAllowedWithBudget(accountID, retryLimit, retryDelay, maxElapsed, false, nil)
}

func (s *FailoverState) sameAccountRetryAllowedWithBudget(accountID int64, retryLimit int, retryDelay, maxElapsed time.Duration, sharedRaceBudget bool, failoverErr *service.UpstreamFailoverError) bool {
	if retryLimit <= 0 || s.SameAccountRetryCount[accountID] >= retryLimit {
		return false
	}
	if failoverErr != nil && failoverErr.RetryRuleKey != "" && failoverErr.RetryRuleLimit >= 0 {
		if s.SameAccountRetryRuleCount == nil {
			s.SameAccountRetryRuleCount = make(sameAccountRetryRuleCounts)
		}
		if s.SameAccountRetryRuleCount[accountID] != nil && s.SameAccountRetryRuleCount[accountID][failoverErr.RetryRuleKey] >= failoverErr.RetryRuleLimit {
			return false
		}
	}
	if s.SameAccountRetryStart == nil {
		s.SameAccountRetryStart = make(map[int64]time.Time)
	}
	if !sharedRaceBudget && !s.SameAccountRetryStart[sharedRaceRetryDeadlineKey].IsZero() {
		sharedRaceBudget = true
	}
	now := time.Now()
	if sharedRaceBudget {
		deadline := s.SameAccountRetryStart[sharedRaceRetryDeadlineKey]
		if deadline.IsZero() {
			if maxElapsed <= 0 {
				return true
			}
			startedAt := s.SameAccountRetryStart[accountID]
			if startedAt.IsZero() {
				startedAt = now
			}
			deadline = startedAt.Add(maxElapsed)
			s.SameAccountRetryStart[sharedRaceRetryStartedKey] = startedAt
			s.SameAccountRetryStart[sharedRaceRetryDeadlineKey] = deadline
			runtimeops.ObservePreemptionStarted()
			if accountID > 0 {
				s.SameAccountRetryStart[accountID] = startedAt
			}
		} else if s.SameAccountRetryStart[sharedRaceRetryStartedKey].IsZero() {
			if maxElapsed > 0 {
				s.SameAccountRetryStart[sharedRaceRetryStartedKey] = deadline.Add(-maxElapsed)
			} else {
				s.SameAccountRetryStart[sharedRaceRetryStartedKey] = now
			}
		}
		if !s.SameAccountRetryStart[sharedRaceRetryExhaustedKey].IsZero() {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if s.SameAccountRetryStart[sharedRaceRetryExhaustedKey].IsZero() {
				s.SameAccountRetryStart[sharedRaceRetryExhaustedKey] = now
				runtimeops.ObservePreemptionExhausted()
			}
			return false
		}
		if retryDelay >= remaining {
			return false
		}
		return true
	}
	if maxElapsed <= 0 {
		return true
	}
	startedAt := s.SameAccountRetryStart[accountID]
	if startedAt.IsZero() {
		startedAt = now
		s.SameAccountRetryStart[accountID] = startedAt
	}
	elapsed := now.Sub(startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed < maxElapsed && retryDelay < maxElapsed-elapsed
}

// HandleSelectionExhausted 处理选号失败（所有候选账号都在排除列表中）时的退避重试决策。
// 针对 Antigravity 单账号分组的 503 (MODEL_CAPACITY_EXHAUSTED) 场景：
// 清除排除列表、等待退避后重新选号。
//
// 返回 FailoverContinue 时，调用方应设置 SingleAccountRetry context 并 continue。
// 返回 FailoverExhausted 时，调用方应返回错误响应。
// 返回 FailoverCanceled 时，调用方应直接 return。
func (s *FailoverState) HandleSelectionExhausted(ctx context.Context) FailoverAction {
	if ctx != nil && ctx.Err() != nil {
		return FailoverCanceled
	}
	if s.LastFailoverErr != nil &&
		s.LastFailoverErr.StatusCode == http.StatusServiceUnavailable &&
		s.SwitchCount <= s.MaxSwitches {

		logger.FromContext(ctx).Warn("gateway.failover_single_account_backoff",
			zap.Duration("backoff_delay", singleAccountBackoffDelay),
			zap.Int("switch_count", s.SwitchCount),
			zap.Int("max_switches", s.MaxSwitches),
		)
		if !sleepWithContext(ctx, singleAccountBackoffDelay) {
			return FailoverCanceled
		}
		logger.FromContext(ctx).Warn("gateway.failover_single_account_retry",
			zap.Int("switch_count", s.SwitchCount),
			zap.Int("max_switches", s.MaxSwitches),
		)
		s.FailedAccountIDs = make(map[int64]struct{})
		return FailoverContinue
	}
	return FailoverExhausted
}

func failoverClientGone(c *gin.Context) bool {
	if c == nil || c.Request == nil || !errors.Is(c.Request.Context().Err(), context.Canceled) {
		return false
	}
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		return true
	}
	if !c.Writer.Written() {
		c.Status(statusClientClosedRequest)
	}
	return true
}

// needForceCacheBilling 判断 failover 时是否需要强制缓存计费。
// 粘性会话实际切换账号、或上游明确标记时，将 input_tokens 转为 cache_read 计费。
func needForceCacheBilling(hasBoundSession bool, failoverErr *service.UpstreamFailoverError, sameAccountRetry bool) bool {
	return (hasBoundSession && !sameAccountRetry) || (failoverErr != nil && failoverErr.ForceCacheBilling)
}

func isOpenAIPoolModelRoutingFailover(account *service.Account, failoverErr *service.UpstreamFailoverError) bool {
	if account == nil || failoverErr == nil || !account.IsOpenAI() || !account.IsPoolMode() {
		return false
	}
	if !failoverErr.SkipPoolSoftCooldown {
		return false
	}
	return service.IsOpenAIPoolModelRoutingError(failoverErr.StatusCode, failoverErr.Message, failoverErr.ResponseBody)
}

func lockOpenAIModelRoutingFailoverPriority(current int, account *service.Account, failoverErr *service.UpstreamFailoverError, protectionEnabled bool) int {
	if current >= 0 || !protectionEnabled || !isOpenAIPoolModelRoutingFailover(account, failoverErr) {
		return current
	}
	return account.Priority
}

// sleepWithContext 等待指定时长，返回 false 表示 context 已取消。
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
