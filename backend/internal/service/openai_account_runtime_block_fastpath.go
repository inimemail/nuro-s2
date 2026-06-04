package service

import (
	"context"
	"net/http"
	"strings"
	"time"
)

const (
	openAIAccountStateUpdateTimeout       = 5 * time.Second
	openAIOAuth429FallbackCooldown        = 5 * time.Second
	openAIPoolSoftCooldownDefault         = 10 * time.Second
	openAIPoolSoftCooldownAuth            = 10 * time.Minute
	openAIPoolSoftCooldownServerError     = 30 * time.Second
	openAIStopSchedulingBridgeCooldown    = 2 * time.Minute
	openAIOAuth429StormWindow             = 10 * time.Second
	openAIOAuth429StormThreshold          = 20
	openAIOAuth429StormMaxAccountSwitches = 1
)

func openAIAccountStateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(base, openAIAccountStateUpdateTimeout)
}

func isOpenAIOAuthAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth
}

func isOpenAIAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI
}

func (s *OpenAIGatewayService) handleOpenAIAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte, requestedModel ...string) bool {
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()

	if statusCode == http.StatusTooManyRequests {
		s.markOpenAIOAuth429RateLimited(stateCtx, account, headers, responseBody)
	}
	if s == nil || account == nil || s.rateLimitService == nil {
		return false
	}
	if len(requestedModel) > 0 && s.rateLimitService.HandleUpstreamModelNotFound(stateCtx, account, requestedModel[0], statusCode, responseBody) {
		return true
	}
	shouldDisable := s.rateLimitService.HandleUpstreamError(stateCtx, account, statusCode, headers, responseBody)
	if shouldDisable {
		s.BlockAccountScheduling(account, time.Time{}, "upstream_disable")
	}
	return shouldDisable
}

func (s *OpenAIGatewayService) markOpenAIOAuth429RateLimited(ctx context.Context, account *Account, headers http.Header, responseBody []byte) {
	if s == nil || !isOpenAIOAuthAccount(account) {
		return
	}
	s.recordOpenAIOAuth429()

	cooldownUntil := time.Now().Add(openAIOAuth429FallbackCooldown)
	if s.rateLimitService != nil {
		if resetAt := s.rateLimitService.calculateOpenAI429ResetTime(headers); resetAt != nil && resetAt.After(time.Now()) {
			cooldownUntil = *resetAt
		} else if resetUnix := parseOpenAIRateLimitResetTime(responseBody); resetUnix != nil {
			if resetAt := time.Unix(*resetUnix, 0); resetAt.After(time.Now()) {
				cooldownUntil = resetAt
			}
		} else if cooldown, ok := s.rateLimitService.get429FallbackCooldown(ctx, account); ok && cooldown > 0 {
			cooldownUntil = time.Now().Add(cooldown)
		}
	}
	s.BlockAccountScheduling(account, cooldownUntil, "429")
}

func (s *OpenAIGatewayService) BlockAccountScheduling(account *Account, until time.Time, reason string) {
	if s == nil || !isOpenAIAccount(account) {
		return
	}
	now := time.Now()
	blockUntil := until
	if blockUntil.IsZero() || !blockUntil.After(now) {
		blockUntil = now.Add(openAIStopSchedulingBridgeCooldown)
	}

	for {
		current, loaded := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
		if !loaded {
			actual, stored := s.openaiAccountRuntimeBlockUntil.LoadOrStore(account.ID, blockUntil)
			if !stored {
				return
			}
			current = actual
		}

		currentUntil, ok := current.(time.Time)
		if !ok || currentUntil.IsZero() {
			if s.openaiAccountRuntimeBlockUntil.CompareAndSwap(account.ID, current, blockUntil) {
				return
			}
			continue
		}
		if currentUntil.After(blockUntil) {
			return
		}
		if s.openaiAccountRuntimeBlockUntil.CompareAndSwap(account.ID, current, blockUntil) {
			return
		}
	}
}

func (s *OpenAIGatewayService) ClearAccountSchedulingBlock(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openaiAccountRuntimeBlockUntil.Delete(accountID)
	s.openaiPoolSoftCooldownUntil.Delete(accountID)
}

func (s *OpenAIGatewayService) isOpenAIAccountRuntimeBlocked(account *Account) bool {
	if s == nil || !isOpenAIAccount(account) {
		return false
	}
	value, ok := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	if !ok {
		return false
	}
	cooldownUntil, ok := value.(time.Time)
	if !ok || cooldownUntil.IsZero() {
		s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
		return false
	}
	if time.Now().Before(cooldownUntil) {
		return true
	}
	s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
	return false
}

func (s *OpenAIGatewayService) MarkOpenAIPoolAccountSoftCooldown(ctx context.Context, account *Account, statusCode int, responseBody []byte) {
	if s == nil || account == nil || !account.IsOpenAI() || !account.IsPoolMode() {
		return
	}
	cooldown := openAIPoolSoftCooldownDefault
	switch {
	case statusCode == http.StatusTooManyRequests:
		if s.rateLimitService != nil {
			if resetAt := parseOpenAIRateLimitResetTime(responseBody); resetAt != nil {
				if until := time.Unix(*resetAt, 0); until.After(time.Now()) {
					cooldown = time.Until(until)
				}
			} else if configured, ok := s.rateLimitService.get429FallbackCooldown(ctx, account); ok && configured > 0 {
				cooldown = configured
			}
		}
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		cooldown = openAIPoolSoftCooldownAuth
	case statusCode >= 500:
		cooldown = openAIPoolSoftCooldownServerError
	}
	if cooldown <= 0 {
		return
	}
	until := time.Now().Add(cooldown)
	for {
		current, loaded := s.openaiPoolSoftCooldownUntil.Load(account.ID)
		if !loaded {
			if _, stored := s.openaiPoolSoftCooldownUntil.LoadOrStore(account.ID, until); !stored {
				return
			}
			continue
		}
		currentUntil, ok := current.(time.Time)
		if !ok || currentUntil.Before(until) {
			if s.openaiPoolSoftCooldownUntil.CompareAndSwap(account.ID, current, until) {
				return
			}
			continue
		}
		return
	}
}

func (s *OpenAIGatewayService) isOpenAIPoolAccountSoftCooling(account *Account) bool {
	if s == nil || account == nil || !account.IsOpenAI() || !account.IsPoolMode() {
		return false
	}
	value, ok := s.openaiPoolSoftCooldownUntil.Load(account.ID)
	if !ok {
		return false
	}
	until, ok := value.(time.Time)
	if !ok || until.IsZero() {
		s.openaiPoolSoftCooldownUntil.Delete(account.ID)
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	s.openaiPoolSoftCooldownUntil.Delete(account.ID)
	return false
}

func (s *OpenAIGatewayService) openAIPoolAccountSoftCooldownUntil(account *Account) (time.Time, bool) {
	if s == nil || account == nil || !account.IsOpenAI() || !account.IsPoolMode() {
		return time.Time{}, false
	}
	value, ok := s.openaiPoolSoftCooldownUntil.Load(account.ID)
	if !ok {
		return time.Time{}, false
	}
	until, ok := value.(time.Time)
	if !ok || until.IsZero() {
		s.openaiPoolSoftCooldownUntil.Delete(account.ID)
		return time.Time{}, false
	}
	if time.Now().Before(until) {
		return until, true
	}
	s.openaiPoolSoftCooldownUntil.Delete(account.ID)
	return time.Time{}, false
}

func (s *OpenAIGatewayService) HandleOpenAIAccountFailoverSwitch(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	account *Account,
	failoverErr *UpstreamFailoverError,
) {
	if s == nil || account == nil {
		return
	}
	if failoverErr != nil {
		s.MarkOpenAIPoolAccountSoftCooldown(ctx, account, failoverErr.StatusCode, failoverErr.ResponseBody)
	}
	if strings.TrimSpace(sessionHash) == "" {
		return
	}
	boundAccountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash)
	if err == nil && boundAccountID == account.ID {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
	}
}

func (s *OpenAIGatewayService) hasHigherPriorityOpenAIAccountAvailable(
	ctx context.Context,
	groupID *int64,
	current *Account,
	requestedModel string,
	requireCompact bool,
	requiredCapability OpenAIEndpointCapability,
	requiredTransport OpenAIUpstreamTransport,
) bool {
	if s == nil || current == nil {
		return false
	}
	accounts, err := s.listSchedulableAccounts(ctx, groupID)
	if err != nil || len(accounts) == 0 {
		return false
	}

	candidates := make([]*Account, 0, len(accounts))
	loadReq := make([]AccountWithConcurrency, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if account.ID == current.ID || account.Priority >= current.Priority {
			continue
		}
		if !isOpenAIAccountEligibleForRequest(ctx, account, requestedModel, requireCompact, requiredCapability) {
			continue
		}
		if s.isOpenAIAccountRuntimeBlocked(account) || s.isOpenAIPoolAccountSoftCooling(account) {
			continue
		}
		if requiredTransport != OpenAIUpstreamTransportAny &&
			requiredTransport != OpenAIUpstreamTransportHTTPSSE &&
			!s.isOpenAIAccountTransportCompatible(account, requiredTransport) {
			continue
		}
		if groupID != nil && s.needsUpstreamChannelRestrictionCheck(ctx, groupID) &&
			s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, requestedModel, requireCompact) {
			continue
		}
		candidates = append(candidates, account)
		loadReq = append(loadReq, AccountWithConcurrency{ID: account.ID, MaxConcurrency: account.EffectiveLoadFactor()})
	}
	if len(candidates) == 0 {
		return false
	}
	if s.concurrencyService == nil {
		return true
	}
	loadMap, err := s.concurrencyService.GetAccountsLoadBatch(ctx, loadReq)
	if err != nil {
		return false
	}
	for _, candidate := range candidates {
		loadInfo := loadMap[candidate.ID]
		if loadInfo == nil || loadInfo.LoadRate < 100 {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) recordOpenAIOAuth429() {
	if s == nil {
		return
	}
	now := time.Now()
	windowStart := s.openaiOAuth429WindowStartUnixNano.Load()
	if windowStart == 0 || now.Sub(time.Unix(0, windowStart)) >= openAIOAuth429StormWindow {
		if s.openaiOAuth429WindowStartUnixNano.CompareAndSwap(windowStart, now.UnixNano()) {
			s.openaiOAuth429WindowCount.Store(1)
			return
		}
	}
	s.openaiOAuth429WindowCount.Add(1)
}

func (s *OpenAIGatewayService) isOpenAIOAuth429Storm() bool {
	if s == nil {
		return false
	}
	windowStart := s.openaiOAuth429WindowStartUnixNano.Load()
	if windowStart == 0 || time.Since(time.Unix(0, windowStart)) >= openAIOAuth429StormWindow {
		return false
	}
	return s.openaiOAuth429WindowCount.Load() >= openAIOAuth429StormThreshold
}

func (s *OpenAIGatewayService) ShouldStopOpenAIOAuth429Failover(account *Account, statusCode int, failedSwitches int) bool {
	if statusCode != http.StatusTooManyRequests || failedSwitches < openAIOAuth429StormMaxAccountSwitches {
		return false
	}
	if !isOpenAIOAuthAccount(account) {
		return false
	}
	return s.isOpenAIOAuth429Storm()
}
