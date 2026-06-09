package service

import (
	"context"
	"time"
)

type compositeAccountRuntimeBlocker struct {
	blockers []AccountRuntimeBlocker
}

func NewCompositeAccountRuntimeBlocker(openai *OpenAIGatewayService, anthropic *GatewayService, rateLimitService *RateLimitService, openAITokenProvider *OpenAITokenProvider) AccountRuntimeBlocker {
	blockers := make([]AccountRuntimeBlocker, 0, 2)
	if openai != nil {
		blockers = append(blockers, openai)
	}
	if anthropic != nil {
		blockers = append(blockers, anthropic)
	}
	blocker := &compositeAccountRuntimeBlocker{blockers: blockers}
	if rateLimitService != nil {
		rateLimitService.SetAccountRuntimeBlocker(blocker)
	}
	if openAITokenProvider != nil {
		openAITokenProvider.SetAccountRuntimeBlocker(blocker)
	}
	return blocker
}

func (b *compositeAccountRuntimeBlocker) BlockAccountScheduling(account *Account, until time.Time, reason string) {
	if b == nil {
		return
	}
	for _, blocker := range b.blockers {
		if blocker != nil {
			blocker.BlockAccountScheduling(account, until, reason)
		}
	}
}

func (b *compositeAccountRuntimeBlocker) ClearAccountSchedulingBlock(accountID int64) {
	if b == nil {
		return
	}
	for _, blocker := range b.blockers {
		if blocker != nil {
			blocker.ClearAccountSchedulingBlock(accountID)
		}
	}
}

func (b *compositeAccountRuntimeBlocker) OpenAIPoolSoftCooldownState(accountID int64) OpenAIPoolSoftCooldownState {
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

func (b *compositeAccountRuntimeBlocker) MaybeKickOpenAIPoolRecoveryProbeFromAdminList(ctx context.Context, account *Account) {
	if b == nil {
		return
	}
	for _, blocker := range b.blockers {
		if kicker, ok := blocker.(openAIPoolRecoveryProbeAdminKicker); ok {
			kicker.MaybeKickOpenAIPoolRecoveryProbeFromAdminList(ctx, account)
		}
	}
}

func (b *compositeAccountRuntimeBlocker) AnthropicPoolSoftCooldownState(accountID int64) AnthropicPoolSoftCooldownState {
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

func (b *compositeAccountRuntimeBlocker) MaybeKickAnthropicPoolRecoveryProbeFromAdminList(ctx context.Context, account *Account) {
	if b == nil {
		return
	}
	for _, blocker := range b.blockers {
		if kicker, ok := blocker.(anthropicPoolRecoveryProbeAdminKicker); ok {
			kicker.MaybeKickAnthropicPoolRecoveryProbeFromAdminList(ctx, account)
		}
	}
}
