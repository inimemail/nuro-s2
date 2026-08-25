package service

import (
	"context"
	"errors"
	"sync"
	"time"
)

const successfulProbeStateRecoveryTimeout = 5 * time.Second

type accountRuntimeDeadline struct {
	Until           time.Time
	ClearGeneration int64
}

func parseAccountRuntimeDeadline(value any) (time.Time, int64, bool) {
	switch deadline := value.(type) {
	case accountRuntimeDeadline:
		return deadline.Until, deadline.ClearGeneration, !deadline.Until.IsZero()
	case time.Time:
		return deadline, 0, !deadline.IsZero()
	default:
		return time.Time{}, 0, false
	}
}

func accountRuntimeDeadlineGeneration(values *sync.Map, accountID int64, expectedUntil time.Time) (int64, bool) {
	if values == nil || accountID <= 0 || expectedUntil.IsZero() {
		return 0, false
	}
	value, ok := values.Load(accountID)
	if !ok {
		return 0, false
	}
	until, generation, valid := parseAccountRuntimeDeadline(value)
	return generation, valid && until.Equal(expectedUntil)
}

func replaceAccountRuntimeDeadlineIfMatches(values *sync.Map, accountID int64, expectedUntil time.Time, expectedGeneration int64, nextUntil time.Time) bool {
	if values == nil || accountID <= 0 || expectedUntil.IsZero() || nextUntil.IsZero() {
		return false
	}
	for {
		value, ok := values.Load(accountID)
		if !ok {
			return false
		}
		until, generation, valid := parseAccountRuntimeDeadline(value)
		if !valid || !until.Equal(expectedUntil) || generation != expectedGeneration {
			return false
		}
		next := accountRuntimeDeadline{Until: nextUntil, ClearGeneration: expectedGeneration}
		if values.CompareAndSwap(accountID, value, next) {
			return true
		}
	}
}

func deleteAccountRuntimeDeadlineIfMatches(values *sync.Map, accountID int64, expectedUntil time.Time, expectedGeneration int64) bool {
	if values == nil || accountID <= 0 || expectedUntil.IsZero() {
		return false
	}
	for {
		value, ok := values.Load(accountID)
		if !ok {
			return false
		}
		until, generation, valid := parseAccountRuntimeDeadline(value)
		if !valid || !until.Equal(expectedUntil) || generation != expectedGeneration {
			return false
		}
		if values.CompareAndDelete(accountID, value) {
			return true
		}
	}
}

func deleteAccountRuntimeDeadlineBefore(values *sync.Map, accountID, clearGeneration int64) bool {
	if values == nil || accountID <= 0 {
		return false
	}
	for {
		value, ok := values.Load(accountID)
		if !ok {
			return true
		}
		_, generation, valid := parseAccountRuntimeDeadline(value)
		if valid && clearGeneration > 0 && generation >= clearGeneration {
			return false
		}
		if values.CompareAndDelete(accountID, value) {
			return true
		}
	}
}

type CompositeAccountRuntimeBlocker struct {
	blockers []AccountRuntimeBlocker
}

type accountRuntimeGenerationMatcher interface {
	accountRuntimeClearGenerationMatches(accountID, expectedGeneration int64) bool
}

func (b *CompositeAccountRuntimeBlocker) localRuntimeGenerationMatches(accountID, expectedGeneration int64) bool {
	if b == nil {
		return false
	}
	for _, blocker := range b.blockers {
		matcher, ok := blocker.(accountRuntimeGenerationMatcher)
		if !ok {
			continue
		}
		if !matcher.accountRuntimeClearGenerationMatches(accountID, expectedGeneration) {
			return false
		}
	}
	// With no generation-aware blocker, retain legacy local-clear behavior.
	// When a matcher is present, reaching this point means every matcher
	// agreed that the expected generation is still current.
	return true
}

func (b *CompositeAccountRuntimeBlocker) AccountRuntimeRecoveryFenceMatches(accountID int64, expectedUntil time.Time, expectedGeneration int64) bool {
	if b == nil || accountID <= 0 || expectedUntil.IsZero() {
		return false
	}
	for _, blocker := range b.blockers {
		if fencer, ok := blocker.(interface {
			AccountRuntimeRecoveryFenceMatches(int64, time.Time, int64) bool
		}); ok {
			return fencer.AccountRuntimeRecoveryFenceMatches(accountID, expectedUntil, expectedGeneration)
		}
	}
	return true
}

func NewCompositeAccountRuntimeBlocker(openai *OpenAIGatewayService, anthropic *GatewayService, rateLimitService *RateLimitService, openAITokenProvider *OpenAITokenProvider) *CompositeAccountRuntimeBlocker {
	blockers := make([]AccountRuntimeBlocker, 0, 2)
	if openai != nil {
		blockers = append(blockers, openai)
	}
	if anthropic != nil {
		blockers = append(blockers, anthropic)
	}
	blocker := &CompositeAccountRuntimeBlocker{blockers: blockers}
	if rateLimitService != nil {
		rateLimitService.SetAccountRuntimeBlocker(blocker)
	}
	if openAITokenProvider != nil {
		openAITokenProvider.SetAccountRuntimeBlocker(blocker)
	}
	return blocker
}

func (b *CompositeAccountRuntimeBlocker) BlockAccountScheduling(account *Account, until time.Time, reason string) {
	if b == nil {
		return
	}
	for _, blocker := range b.blockers {
		if blocker != nil {
			blocker.BlockAccountScheduling(account, until, reason)
		}
	}
}

func (b *CompositeAccountRuntimeBlocker) ClearAccountSchedulingBlock(accountID int64) {
	if b == nil {
		return
	}
	for _, blocker := range b.blockers {
		if blocker != nil {
			blocker.ClearAccountSchedulingBlock(accountID)
		}
	}
}

// ClearAccountSchedulingBlockAcrossReplicas advances the shared generation
// before clearing. Admin and successful-probe recovery use this path so every
// replica drops old state while cooldowns created at the new generation remain.
func (b *CompositeAccountRuntimeBlocker) ClearAccountSchedulingBlockAcrossReplicas(ctx context.Context, accountID int64) error {
	_, err := b.clearAccountSchedulingBlockAcrossReplicas(ctx, accountID, nil)
	return err
}

// ClearAccountSchedulingBlockAcrossReplicasIfGeneration clears only when the
// shared runtime-clear generation still matches the probe's starting point.
// A stale replica therefore cannot clear a newer cooldown.
func (b *CompositeAccountRuntimeBlocker) ClearAccountSchedulingBlockAcrossReplicasIfGeneration(ctx context.Context, accountID, expectedGeneration int64) (bool, error) {
	advanced, err := b.clearAccountSchedulingBlockAcrossReplicas(ctx, accountID, &expectedGeneration)
	return advanced, err
}

func (b *CompositeAccountRuntimeBlocker) clearAccountSchedulingBlockAcrossReplicas(ctx context.Context, accountID int64, expectedGeneration *int64) (bool, error) {
	if b == nil || accountID <= 0 {
		return false, nil
	}
	var snapshot *SchedulerSnapshotService
	for _, blocker := range b.blockers {
		switch service := blocker.(type) {
		case *OpenAIGatewayService:
			if service != nil && service.schedulerSnapshot != nil {
				snapshot = service.schedulerSnapshot
			}
		case *GatewayService:
			if snapshot == nil && service != nil && service.schedulerSnapshot != nil {
				snapshot = service.schedulerSnapshot
			}
		}
	}
	if snapshot == nil {
		if expectedGeneration != nil {
			if !b.localRuntimeGenerationMatches(accountID, *expectedGeneration) {
				return false, nil
			}
			// Deployments without a scheduler snapshot cannot provide shared
			// generation fencing. Preserve the legacy local-clear behavior
			// instead of leaving a recovered account permanently cooling.
			b.ClearAccountSchedulingBlock(accountID)
			return true, nil
		}
		b.ClearAccountSchedulingBlock(accountID)
		return true, nil
	}
	// A snapshot can exist while event propagation is disabled (or while a
	// legacy bus does not expose generation storage). There is no remote state
	// to fence in that mode, so retain the established local-clear behavior
	// instead of returning a publish error and leaving the account cooling.
	if _, hasGenerationStore := snapshot.eventBus.(SchedulerRuntimeClearGenerationStore); !hasGenerationStore {
		if expectedGeneration != nil && !b.localRuntimeGenerationMatches(accountID, *expectedGeneration) {
			return false, nil
		}
		b.ClearAccountSchedulingBlock(accountID)
		return true, nil
	}
	var generation int64
	var advanced bool
	var err error
	if expectedGeneration != nil {
		if _, supportsCAS := snapshot.eventBus.(SchedulerRuntimeClearGenerationCASStore); supportsCAS {
			generation, advanced, err = snapshot.publishAccountRuntimeClearIfGeneration(ctx, accountID, *expectedGeneration)
		} else {
			// Preserve compatibility with legacy event buses that predate
			// generation fencing; repository wiring uses the CAS-capable bus.
			generation, err = snapshot.publishAccountRuntimeClear(ctx, accountID)
			advanced = true
		}
		if err != nil || !advanced {
			return advanced, err
		}
	} else {
		generation, err = snapshot.publishAccountRuntimeClear(ctx, accountID)
		advanced = true
	}
	if generation <= 0 {
		if err != nil {
			return false, err
		}
		if expectedGeneration != nil {
			return false, nil
		}
		b.ClearAccountSchedulingBlock(accountID)
		return true, nil
	}
	for _, blocker := range b.blockers {
		switch service := blocker.(type) {
		case *OpenAIGatewayService:
			service.clearLocalAccountSchedulingBlockBefore(accountID, generation)
			if clearErr := service.clearOpenAIAccountCooldownInRedisBefore(accountID, generation); clearErr != nil {
				err = errors.Join(err, clearErr)
			}
			if clearErr := service.clearOpenAIPoolCooldownInRedisBefore(accountID, generation); clearErr != nil {
				err = errors.Join(err, clearErr)
			}
		case *GatewayService:
			service.clearAnthropicPoolSoftCooldownBefore(accountID, generation)
		default:
			blocker.ClearAccountSchedulingBlock(accountID)
		}
	}
	return advanced, err
}

func (b *CompositeAccountRuntimeBlocker) OpenAIPoolSoftCooldownState(accountID int64) OpenAIPoolSoftCooldownState {
	if b == nil {
		return OpenAIPoolSoftCooldownState{}
	}
	for _, blocker := range b.blockers {
		if reader, ok := blocker.(openAIPoolSoftCooldownStateReader); ok {
			if state := reader.OpenAIPoolSoftCooldownState(accountID); state.Cooling {
				return state
			}
		}
	}
	return OpenAIPoolSoftCooldownState{}
}

func (b *CompositeAccountRuntimeBlocker) OpenAIPoolSoftCooldownStateForAccount(ctx context.Context, account *Account) OpenAIPoolSoftCooldownState {
	if b == nil || account == nil {
		return OpenAIPoolSoftCooldownState{}
	}
	for _, blocker := range b.blockers {
		if reader, ok := blocker.(openAIPoolSoftCooldownAccountStateReader); ok {
			if state := reader.OpenAIPoolSoftCooldownStateForAccount(ctx, account); state.Cooling {
				return state
			}
		}
	}
	return b.OpenAIPoolSoftCooldownState(account.ID)
}

func (b *CompositeAccountRuntimeBlocker) MaybeKickOpenAIPoolRecoveryProbeFromAdminList(ctx context.Context, account *Account) {
	if b == nil {
		return
	}
	for _, blocker := range b.blockers {
		if kicker, ok := blocker.(openAIPoolRecoveryProbeAdminKicker); ok {
			kicker.MaybeKickOpenAIPoolRecoveryProbeFromAdminList(ctx, account)
		}
	}
}

func (b *CompositeAccountRuntimeBlocker) AnthropicPoolSoftCooldownState(accountID int64) AnthropicPoolSoftCooldownState {
	if b == nil {
		return AnthropicPoolSoftCooldownState{}
	}
	for _, blocker := range b.blockers {
		if reader, ok := blocker.(anthropicPoolSoftCooldownStateReader); ok {
			if state := reader.AnthropicPoolSoftCooldownState(accountID); state.Cooling {
				return state
			}
		}
	}
	return AnthropicPoolSoftCooldownState{}
}

func (b *CompositeAccountRuntimeBlocker) AnthropicPoolSoftCooldownStateForAccount(ctx context.Context, account *Account) AnthropicPoolSoftCooldownState {
	if b == nil || account == nil {
		return AnthropicPoolSoftCooldownState{}
	}
	for _, blocker := range b.blockers {
		if reader, ok := blocker.(anthropicPoolSoftCooldownAccountStateReader); ok {
			if state := reader.AnthropicPoolSoftCooldownStateForAccount(ctx, account); state.Cooling {
				return state
			}
		}
	}
	return b.AnthropicPoolSoftCooldownState(account.ID)
}

func (b *CompositeAccountRuntimeBlocker) MaybeKickAnthropicPoolRecoveryProbeFromAdminList(ctx context.Context, account *Account) {
	if b == nil {
		return
	}
	for _, blocker := range b.blockers {
		if kicker, ok := blocker.(anthropicPoolRecoveryProbeAdminKicker); ok {
			kicker.MaybeKickAnthropicPoolRecoveryProbeFromAdminList(ctx, account)
		}
	}
}

func (b *CompositeAccountRuntimeBlocker) NonOpenAIPoolRuntimeStateForAccount(account *Account) NonOpenAIPoolRuntimeState {
	if b == nil || account == nil {
		return NonOpenAIPoolRuntimeState{}
	}
	for _, blocker := range b.blockers {
		if reader, ok := blocker.(interface {
			NonOpenAIPoolRuntimeStateForAccount(*Account) NonOpenAIPoolRuntimeState
		}); ok {
			if state := reader.NonOpenAIPoolRuntimeStateForAccount(account); state.Cooling {
				return state
			}
		}
	}
	return NonOpenAIPoolRuntimeState{}
}
