package service

import (
	"context"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicPoolSoftCooldownDefault     = 5 * time.Second
	anthropicPoolSoftCooldownAuth        = 30 * time.Second
	anthropicPoolSoftCooldownServerError = 5 * time.Second
	anthropicPoolSoftCooldownMaxDefault  = 30 * time.Second
	anthropicPoolProbeDefaultModel       = "claude-sonnet-4-6"
)

type AnthropicPoolSoftCooldownState struct {
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

func (s *GatewayService) MarkAnthropicPoolAccountSoftCooldown(ctx context.Context, account *Account, statusCode int, responseBody []byte, cooldownContext anthropicPoolSoftCooldownContext) {
	if s == nil || !isAnthropicPoolAccount(account) {
		return
	}
	if !account.IsPoolSoftCooldownEnabled() {
		s.clearAnthropicPoolSoftCooldown(account.ID)
		return
	}
	cooldown := anthropicPoolSoftCooldownDefault
	switch {
	case statusCode == http.StatusTooManyRequests:
		cooldown = anthropicPoolSoftCooldownServerError
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		cooldown = anthropicPoolSoftCooldownAuth
	case statusCode == 529 || statusCode >= 500:
		cooldown = anthropicPoolSoftCooldownServerError
	}
	if cooldown <= 0 {
		return
	}
	cooldown = s.capAnthropicPoolSoftCooldown(ctx, cooldown)
	if cooldownContext.StatusCode == 0 {
		cooldownContext.StatusCode = statusCode
	}
	cooldownContext.ProbeModel = strings.TrimSpace(cooldownContext.ProbeModel)
	if cooldownContext.ProbeModel == "" {
		cooldownContext.ProbeModel = s.anthropicPoolRecoveryProbeModel(ctx)
	}
	cooldownContext.ProbeKind = strings.TrimSpace(cooldownContext.ProbeKind)
	if cooldownContext.ProbeKind == "" {
		cooldownContext.ProbeKind = "messages"
	}
	cooldownContext.CooldownSource = truncateString(strings.TrimSpace(cooldownContext.CooldownSource), 64)
	cooldownContext.Reason = truncateString(strings.TrimSpace(cooldownContext.Reason), 256)
	cooldownContext.LastProbeReason = truncateString(strings.TrimSpace(cooldownContext.LastProbeReason), 256)
	s.anthropicPoolSoftCooldownContext.Store(account.ID, cooldownContext)
	s.storeAnthropicPoolSoftCooldownUntil(account.ID, time.Now().Add(cooldown))
}

func (s *GatewayService) ClearAccountSchedulingBlock(accountID int64) {
	s.clearAnthropicPoolSoftCooldown(accountID)
}

func (s *GatewayService) BlockAccountScheduling(account *Account, until time.Time, reason string) {
}

func (s *GatewayService) capAnthropicPoolSoftCooldown(ctx context.Context, cooldown time.Duration) time.Duration {
	maxCooldown := s.configuredAnthropicPoolSoftCooldownMax(ctx)
	if cooldown > maxCooldown {
		return maxCooldown
	}
	return cooldown
}

func (s *GatewayService) configuredAnthropicPoolSoftCooldownMax(ctx context.Context) time.Duration {
	maxCooldown := anthropicPoolSoftCooldownMaxDefault
	if s != nil && s.settingService != nil {
		if configured := s.settingService.GetAnthropicPoolSoftCooldownMax(ctx); configured > 0 {
			maxCooldown = configured
		}
	}
	return maxCooldown
}

func (s *GatewayService) storeAnthropicPoolSoftCooldownUntil(accountID int64, until time.Time) {
	if s == nil || accountID <= 0 || until.IsZero() {
		return
	}
	now := time.Now()
	for {
		current, loaded := s.anthropicPoolSoftCooldownUntil.Load(accountID)
		if !loaded {
			if _, stored := s.anthropicPoolSoftCooldownUntil.LoadOrStore(accountID, until); !stored {
				return
			}
			continue
		}
		currentUntil, ok := current.(time.Time)
		if !ok || currentUntil.IsZero() || !currentUntil.After(now) {
			if s.anthropicPoolSoftCooldownUntil.CompareAndSwap(accountID, current, until) {
				return
			}
			continue
		}
		return
	}
}

func (s *GatewayService) isAnthropicPoolAccountSoftCooling(account *Account) bool {
	if s == nil || !isAnthropicPoolAccount(account) {
		return false
	}
	_, ok := s.anthropicPoolAccountSoftCooldownUntil(account)
	return ok
}

func (s *GatewayService) anthropicPoolAccountSoftCooldownUntil(account *Account) (time.Time, bool) {
	return s.anthropicPoolAccountSoftCooldownUntilWithContext(context.Background(), account)
}

func (s *GatewayService) anthropicPoolAccountSoftCooldownUntilWithContext(ctx context.Context, account *Account) (time.Time, bool) {
	if s == nil || !isAnthropicPoolAccount(account) {
		return time.Time{}, false
	}
	until, ok := s.anthropicPoolAccountSoftCooldownUntilByID(account.ID)
	if !ok {
		return time.Time{}, false
	}
	until = s.clampAnthropicPoolSoftCooldownUntil(ctx, account, until)
	if until.IsZero() {
		return time.Time{}, false
	}
	return until, true
}

func (s *GatewayService) anthropicPoolAccountSoftCooldownUntilByID(accountID int64) (time.Time, bool) {
	if s == nil || accountID <= 0 {
		return time.Time{}, false
	}
	value, ok := s.anthropicPoolSoftCooldownUntil.Load(accountID)
	if !ok {
		return time.Time{}, false
	}
	until, ok := value.(time.Time)
	if !ok || until.IsZero() {
		s.clearAnthropicPoolSoftCooldown(accountID)
		return time.Time{}, false
	}
	return until, true
}

func (s *GatewayService) anthropicPoolAccountSoftCooldownContext(accountID int64) anthropicPoolSoftCooldownContext {
	if s == nil || accountID <= 0 {
		return anthropicPoolSoftCooldownContext{}
	}
	value, ok := s.anthropicPoolSoftCooldownContext.Load(accountID)
	if !ok {
		return anthropicPoolSoftCooldownContext{}
	}
	cooldownContext, _ := value.(anthropicPoolSoftCooldownContext)
	return cooldownContext
}

func (s *GatewayService) clampAnthropicPoolSoftCooldownUntil(ctx context.Context, account *Account, until time.Time) time.Time {
	if s == nil || account == nil || until.IsZero() {
		return until
	}
	maxCooldown := s.configuredAnthropicPoolSoftCooldownMax(ctx)
	if maxCooldown <= 0 {
		return until
	}
	maxUntil := time.Now().Add(maxCooldown)
	for current := until; ; {
		if current.IsZero() || !current.After(maxUntil) {
			return current
		}
		if s.anthropicPoolSoftCooldownUntil.CompareAndSwap(account.ID, current, maxUntil) {
			return maxUntil
		}
		value, ok := s.anthropicPoolSoftCooldownUntil.Load(account.ID)
		if !ok {
			return time.Time{}
		}
		next, ok := value.(time.Time)
		if !ok || next.IsZero() {
			s.clearAnthropicPoolSoftCooldown(account.ID)
			return time.Time{}
		}
		current = next
	}
}

func (s *GatewayService) anthropicPoolAccountSoftCooldownMatches(accountID int64, expectedUntil time.Time) bool {
	if expectedUntil.IsZero() {
		return false
	}
	until, ok := s.anthropicPoolAccountSoftCooldownUntilByID(accountID)
	return ok && until.Equal(expectedUntil)
}

func (s *GatewayService) isAnthropicPoolAccountSoftCooldownDue(account *Account) bool {
	until, ok := s.anthropicPoolAccountSoftCooldownUntil(account)
	return ok && !time.Now().Before(until)
}

func (s *GatewayService) clearAnthropicPoolSoftCooldown(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.anthropicPoolSoftCooldownUntil.Delete(accountID)
	s.anthropicPoolSoftCooldownContext.Delete(accountID)
	s.anthropicPoolRecoveryProbeInFlight.Delete(accountID)
	s.anthropicPoolRecoveryProbeFailureCnt.Delete(accountID)
}

func (s *GatewayService) AnthropicPoolSoftCooldownState(accountID int64) AnthropicPoolSoftCooldownState {
	until, cooling := s.anthropicPoolAccountSoftCooldownUntilByID(accountID)
	if !cooling {
		return AnthropicPoolSoftCooldownState{}
	}
	_, probing := s.anthropicPoolRecoveryProbeInFlight.Load(accountID)
	cooldownContext := s.anthropicPoolAccountSoftCooldownContext(accountID)
	return AnthropicPoolSoftCooldownState{
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

func (s *GatewayService) AnthropicPoolSoftCooldownStateForAccount(ctx context.Context, account *Account) AnthropicPoolSoftCooldownState {
	if s == nil || account == nil {
		return AnthropicPoolSoftCooldownState{}
	}
	until, cooling := s.anthropicPoolAccountSoftCooldownUntilWithContext(ctx, account)
	if !cooling {
		return AnthropicPoolSoftCooldownState{}
	}
	_, probing := s.anthropicPoolRecoveryProbeInFlight.Load(account.ID)
	cooldownContext := s.anthropicPoolAccountSoftCooldownContext(account.ID)
	return AnthropicPoolSoftCooldownState{
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

func (s *GatewayService) HandleAnthropicAccountFailoverSwitch(ctx context.Context, groupID *int64, sessionHash string, account *Account, failoverErr *UpstreamFailoverError, requestedModel ...string) {
	if s == nil || !isAnthropicPoolAccount(account) || failoverErr == nil {
		return
	}
	model := strings.TrimSpace(failoverErr.ProbeModel)
	if model == "" && len(requestedModel) > 0 {
		model = strings.TrimSpace(requestedModel[0])
	}
	decision := classifyAnthropicPoolFailover(account, failoverErr.StatusCode, failoverErr.Message, failoverErr.ResponseBody, model)
	if !decision.Failover || decision.SkipSoftCooldown || failoverErr.SkipPoolSoftCooldown {
		return
	}
	if model == "" {
		model = decision.ProbeModel
	}
	if model == "" {
		model = s.anthropicPoolRecoveryProbeModel(ctx)
	}
	s.MarkAnthropicPoolAccountSoftCooldown(ctx, account, failoverErr.StatusCode, failoverErr.ResponseBody, anthropicPoolSoftCooldownContext{
		ProbeModel:     model,
		ProbeKind:      "messages",
		CooldownSource: "upstream_failure",
		StatusCode:     failoverErr.StatusCode,
		Reason:         firstNonEmptyString(failoverErr.Message, decision.SoftCooldownMessage),
	})
}
