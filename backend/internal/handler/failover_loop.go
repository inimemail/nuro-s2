package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
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
	Delay      time.Duration
	Elapsed    time.Duration
	MaxElapsed time.Duration
}

func markSameAccountAttemptStart(starts map[int64]time.Time, account *service.Account, startedAt time.Time) {
	if starts == nil || account == nil || account.ID == 0 || startedAt.IsZero() {
		return
	}
	if _, ok := starts[account.ID]; !ok {
		starts[account.ID] = startedAt
	}
}

func planSameAccountRetry(account *service.Account, counts map[int64]int, starts map[int64]time.Time, delay time.Duration) (sameAccountRetryPlan, bool) {
	return planSameAccountRetryWithMaxElapsed(account, counts, starts, delay, 0)
}

func planSameAccountRetryWithMaxElapsed(account *service.Account, counts map[int64]int, starts map[int64]time.Time, delay, maxElapsed time.Duration) (sameAccountRetryPlan, bool) {
	plan := sameAccountRetryPlan{Delay: delay}
	if account == nil || counts == nil {
		return plan, false
	}
	accountID := account.ID
	plan.RetryLimit = account.GetPoolModeRetryCount()
	if counts[accountID] >= plan.RetryLimit {
		return plan, false
	}
	plan.MaxElapsed = account.GetPoolModeSameAccountRetryMaxElapsed()
	if maxElapsed > 0 && plan.MaxElapsed > maxElapsed {
		plan.MaxElapsed = maxElapsed
	}
	if plan.MaxElapsed > 0 {
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
	return plan, true
}

// FailoverState 跨循环迭代共享的 failover 状态
type FailoverState struct {
	SwitchCount           int
	MaxSwitches           int
	FailedAccountIDs      map[int64]struct{}
	SameAccountRetryCount map[int64]int
	SameAccountRetryStart map[int64]time.Time
	LastFailoverErr       *service.UpstreamFailoverError
	ForceCacheBilling     bool
	hasBoundSession       bool
	pendingRetryAccountID int64
	pendingRetryPlatform  string
	pendingRetryPoolMode  bool
	pendingRetryErr       *service.UpstreamFailoverError
}

// NewFailoverState 创建 failover 状态
func NewFailoverState(maxSwitches int, hasBoundSession bool) *FailoverState {
	return &FailoverState{
		MaxSwitches:           maxSwitches,
		FailedAccountIDs:      make(map[int64]struct{}),
		SameAccountRetryCount: make(map[int64]int),
		SameAccountRetryStart: make(map[int64]time.Time),
		hasBoundSession:       hasBoundSession,
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
	return s.handleFailoverErrorWithRetryPlan(
		ctx,
		gatewayService,
		account.ID,
		account.Platform,
		retryLimit,
		retryDelay,
		maxElapsed,
		account.IsPoolMode(),
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
	if ctx != nil && ctx.Err() != nil {
		return FailoverCanceled
	}
	s.clearPendingSameAccountRetry()
	s.LastFailoverErr = failoverErr

	// 同账号重试不算切换账号，粘性会话只在实际切号时强制缓存计费。
	sameAccountRetry := failoverErr.RetryableOnSameAccount &&
		s.sameAccountRetryAllowed(accountID, retryLimit, retryDelay, retryMaxElapsed)
	if needForceCacheBilling(s.hasBoundSession, failoverErr, sameAccountRetry) {
		s.ForceCacheBilling = true
	}

	// 同账号重试：对 RetryableOnSameAccount 的临时性错误，先在同一账号上重试
	if sameAccountRetry {
		s.SameAccountRetryCount[accountID]++
		s.pendingRetryAccountID = accountID
		s.pendingRetryPlatform = platform
		s.pendingRetryPoolMode = poolMode
		s.pendingRetryErr = failoverErr
		logger.FromContext(ctx).Warn("gateway.failover_same_account_retry",
			zap.Int64("account_id", accountID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("same_account_retry_count", s.SameAccountRetryCount[accountID]),
			zap.Int("same_account_retry_max", retryLimit),
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
	failoverErr := s.pendingRetryErr
	s.clearPendingSameAccountRetry()
	return s.handleFailoverErrorWithRetryPlan(ctx, gatewayService, accountID, platform, 0, 0, 0, poolMode, failoverErr)
}

func (s *FailoverState) sameAccountRetryAllowed(accountID int64, retryLimit int, retryDelay, maxElapsed time.Duration) bool {
	if retryLimit <= 0 || s.SameAccountRetryCount[accountID] >= retryLimit {
		return false
	}
	if maxElapsed <= 0 {
		return true
	}
	if s.SameAccountRetryStart == nil {
		s.SameAccountRetryStart = make(map[int64]time.Time)
	}
	now := time.Now()
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
