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
	openAIPoolSoftCooldownDefault         = 5 * time.Second
	openAIPoolSoftCooldownAuth            = 30 * time.Second
	openAIPoolSoftCooldownServerError     = 5 * time.Second
	openAIPoolSoftCooldownMax             = 30 * time.Second
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

	if isOpenAIImageRateLimitError(statusCode, responseBody) {
		if s != nil && s.rateLimitService != nil {
			_ = s.rateLimitService.HandleOpenAIImageRateLimit(stateCtx, account, statusCode, headers, responseBody)
		}
		return false
	}

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
	s.openaiPoolSoftCooldownContext.Delete(accountID)
	s.openaiPoolRecoveryProbeInFlight.Delete(accountID)
	s.openaiPoolRecoveryProbeFailureCount.Delete(accountID)
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
	s.MarkOpenAIPoolAccountSoftCooldownWithContext(ctx, account, statusCode, responseBody, openAIPoolSoftCooldownContext{})
}

func (s *OpenAIGatewayService) MarkOpenAIPoolAccountSoftCooldownWithContext(ctx context.Context, account *Account, statusCode int, responseBody []byte, cooldownContext openAIPoolSoftCooldownContext) {
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
	case statusCode == 529:
		if s.rateLimitService != nil {
			configured, enabled := s.rateLimitService.get529OverloadCooldown(ctx, account)
			if !enabled {
				return
			}
			if configured > 0 {
				cooldown = configured
			}
		}
	case statusCode >= 500:
		cooldown = openAIPoolSoftCooldownServerError
	}
	if cooldown <= 0 {
		return
	}
	cooldown = capOpenAIPoolSoftCooldown(cooldown)
	if cooldownContext.StatusCode == 0 {
		cooldownContext.StatusCode = statusCode
	}
	cooldownContext.ProbeModel = strings.TrimSpace(cooldownContext.ProbeModel)
	cooldownContext.ProbeKind = strings.TrimSpace(cooldownContext.ProbeKind)
	if cooldownContext.ProbeKind == "" {
		if cooldownContext.ProbeCapability != "" || isOpenAIImageGenerationModel(cooldownContext.ProbeModel) {
			cooldownContext.ProbeKind = "images"
		} else {
			cooldownContext.ProbeKind = "openai"
		}
	}
	if strings.TrimSpace(cooldownContext.CooldownSource) == "" {
		cooldownContext.CooldownSource = "upstream_failure"
	}
	if strings.TrimSpace(cooldownContext.Reason) == "" {
		cooldownContext.Reason = sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(responseBody))
	}
	cooldownContext.Reason = truncateString(strings.TrimSpace(cooldownContext.Reason), 256)
	cooldownContext.LastProbeReason = truncateString(strings.TrimSpace(cooldownContext.LastProbeReason), 256)
	if cooldownContext.ProbeCapability != "" || cooldownContext.ProbeModel != "" || cooldownContext.ProbeKind != "" ||
		cooldownContext.CooldownSource != "" || cooldownContext.StatusCode > 0 || cooldownContext.Reason != "" ||
		cooldownContext.LastProbeStatus > 0 || cooldownContext.LastProbeReason != "" {
		s.openaiPoolSoftCooldownContext.Store(account.ID, cooldownContext)
	}
	s.storeOpenAIPoolSoftCooldownUntil(account.ID, time.Now().Add(cooldown))
}

func capOpenAIPoolSoftCooldown(cooldown time.Duration) time.Duration {
	if cooldown > openAIPoolSoftCooldownMax {
		return openAIPoolSoftCooldownMax
	}
	return cooldown
}

func (s *OpenAIGatewayService) storeOpenAIPoolSoftCooldownUntil(accountID int64, until time.Time) {
	if s == nil || accountID <= 0 || until.IsZero() {
		return
	}
	for {
		current, loaded := s.openaiPoolSoftCooldownUntil.Load(accountID)
		if !loaded {
			if _, stored := s.openaiPoolSoftCooldownUntil.LoadOrStore(accountID, until); !stored {
				return
			}
			continue
		}
		currentUntil, ok := current.(time.Time)
		if !ok || currentUntil.Before(until) {
			if s.openaiPoolSoftCooldownUntil.CompareAndSwap(accountID, current, until) {
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
		s.openaiPoolSoftCooldownContext.Delete(account.ID)
		return false
	}
	return true
}

func (s *OpenAIGatewayService) openAIPoolAccountSoftCooldownUntil(account *Account) (time.Time, bool) {
	if s == nil || account == nil || !account.IsOpenAI() || !account.IsPoolMode() {
		return time.Time{}, false
	}
	return s.openAIPoolAccountSoftCooldownUntilByID(account.ID)
}

func (s *OpenAIGatewayService) openAIPoolAccountSoftCooldownUntilByID(accountID int64) (time.Time, bool) {
	if s == nil || accountID <= 0 {
		return time.Time{}, false
	}
	value, ok := s.openaiPoolSoftCooldownUntil.Load(accountID)
	if !ok {
		return time.Time{}, false
	}
	until, ok := value.(time.Time)
	if !ok || until.IsZero() {
		s.openaiPoolSoftCooldownUntil.Delete(accountID)
		s.openaiPoolSoftCooldownContext.Delete(accountID)
		return time.Time{}, false
	}
	return until, true
}

func (s *OpenAIGatewayService) openAIPoolAccountSoftCooldownContext(accountID int64) openAIPoolSoftCooldownContext {
	if s == nil || accountID <= 0 {
		return openAIPoolSoftCooldownContext{}
	}
	value, ok := s.openaiPoolSoftCooldownContext.Load(accountID)
	if !ok {
		return openAIPoolSoftCooldownContext{}
	}
	cooldownContext, _ := value.(openAIPoolSoftCooldownContext)
	return cooldownContext
}

func (s *OpenAIGatewayService) openAIPoolAccountSoftCooldownMatches(accountID int64, expectedUntil time.Time) bool {
	if expectedUntil.IsZero() {
		return false
	}
	until, ok := s.openAIPoolAccountSoftCooldownUntilByID(accountID)
	if !ok {
		return false
	}
	return until.Equal(expectedUntil)
}

func (s *OpenAIGatewayService) isOpenAIPoolAccountSoftCooldownDue(account *Account) bool {
	until, ok := s.openAIPoolAccountSoftCooldownUntil(account)
	return ok && !time.Now().Before(until)
}

type OpenAIPoolSoftCooldownState struct {
	Until           time.Time
	Cooling         bool
	Due             bool
	ProbeInFlight   bool
	StatusCode      int
	Reason          string
	ProbeModel      string
	ProbeKind       string
	CooldownSource  string
	LastProbeStatus int
	LastProbeReason string
}

func (s *OpenAIGatewayService) OpenAIPoolSoftCooldownState(accountID int64) OpenAIPoolSoftCooldownState {
	until, cooling := s.openAIPoolAccountSoftCooldownUntilByID(accountID)
	if !cooling {
		return OpenAIPoolSoftCooldownState{}
	}
	_, probing := s.openaiPoolRecoveryProbeInFlight.Load(accountID)
	cooldownContext := s.openAIPoolAccountSoftCooldownContext(accountID)
	return OpenAIPoolSoftCooldownState{
		Until:           until,
		Cooling:         true,
		Due:             !time.Now().Before(until),
		ProbeInFlight:   probing,
		StatusCode:      cooldownContext.StatusCode,
		Reason:          cooldownContext.Reason,
		ProbeModel:      cooldownContext.ProbeModel,
		ProbeKind:       cooldownContext.ProbeKind,
		CooldownSource:  cooldownContext.CooldownSource,
		LastProbeStatus: cooldownContext.LastProbeStatus,
		LastProbeReason: cooldownContext.LastProbeReason,
	}
}

func (s *OpenAIGatewayService) HandleOpenAIAccountFailoverSwitch(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	account *Account,
	failoverErr *UpstreamFailoverError,
	requestedModel ...string,
) {
	if s == nil || account == nil {
		return
	}
	if failoverErr != nil {
		decision := classifyOpenAIPoolFailover(account, failoverErr.StatusCode, failoverErr.Message, failoverErr.ResponseBody)
		userRequestError := isOpenAIPoolUserRequestedModelError(failoverErr.StatusCode, failoverErr.Message, failoverErr.ResponseBody) ||
			isOpenAIPoolExplicitClientRequestError(failoverErr.StatusCode, failoverErr.Message, failoverErr.ResponseBody)
		if !userRequestError && !failoverErr.SkipPoolSoftCooldown && !decision.SkipSoftCooldown {
			probeModel := strings.TrimSpace(failoverErr.ProbeModel)
			if probeModel == "" && len(requestedModel) > 0 {
				probeModel = strings.TrimSpace(requestedModel[0])
			}
			probeKind := strings.TrimSpace(failoverErr.ProbeKind)
			if probeKind == "" {
				if account.IsImagePoolMode() {
					probeKind = "images"
				} else {
					probeKind = openAIPoolProbeKindForModel(probeModel)
				}
			}
			probeCapability := failoverErr.ProbeCapability
			if probeCapability == "" {
				probeCapability = decision.ProbeCapability
			}
			s.MarkOpenAIPoolAccountSoftCooldownWithContext(ctx, account, failoverErr.StatusCode, failoverErr.ResponseBody, openAIPoolSoftCooldownContext{
				ProbeCapability: probeCapability,
				ProbeModel:      probeModel,
				ProbeKind:       probeKind,
				CooldownSource:  "upstream_failure",
				StatusCode:      failoverErr.StatusCode,
				Reason:          failoverErr.Message,
			})
		}
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
	requiredImageCapability OpenAIImagesCapability,
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
		if !isOpenAIAccountEligibleForRequest(ctx, account, requestedModel, requireCompact, requiredCapability, requiredImageCapability) {
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
