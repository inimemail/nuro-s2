package service

import (
	"context"
	"strconv"
	"strings"
	"time"
)

const (
	openAIPromptCacheCreationUnsupportedDisableTTL = 10 * time.Minute
	openAIPromptCacheCreationRemotePollInterval    = time.Second
	openAIPromptCacheCreationDisabledRedisPrefix   = "openai:prompt_cache_creation_disabled:v1:"
)

// IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled applies the shared
// compatibility backoff without changing the account's configured setting.
func (s *OpenAIGatewayService) IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account *Account) bool {
	if account == nil || !account.IsOpenAIPromptCacheCreationOptimizationEnabled() {
		return false
	}
	if s == nil {
		return true
	}
	now := time.Now()
	if raw, ok := s.openaiPromptCacheCreationDisabledUntil.Load(account.ID); ok {
		if until, ok := raw.(time.Time); ok && now.Before(until) {
			return false
		}
		s.openaiPromptCacheCreationDisabledUntil.Delete(account.ID)
	}
	if s.openaiAccountHealthRedis == nil || !s.shouldPollPromptCacheCreationDisabledState(account.ID, now) {
		return true
	}
	s.openaiPromptCacheCreationRemoteCheckedAt.Store(account.ID, now)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	raw, err := s.openaiAccountHealthRedis.HGet(ctx, openAIPromptCacheCreationDisabledRedisPrefix+strconv.FormatInt(account.ID, 10), "disabled_until").Result()
	cancel()
	if err != nil {
		return true
	}
	untilUnix, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || untilUnix <= now.Unix() {
		return true
	}
	until := time.Unix(untilUnix, 0)
	s.openaiPromptCacheCreationDisabledUntil.Store(account.ID, until)
	return false
}

func (s *OpenAIGatewayService) shouldPollPromptCacheCreationDisabledState(accountID int64, now time.Time) bool {
	last, ok := s.openaiPromptCacheCreationRemoteCheckedAt.Load(accountID)
	if !ok {
		return true
	}
	checked, ok := last.(time.Time)
	return !ok || now.Sub(checked) >= openAIPromptCacheCreationRemotePollInterval
}

// ApplyOpenAIPromptCacheCreationOptimizationBody is the service-aware wrapper
// used by request paths. The pure helper remains available for unit tests.
func (s *OpenAIGatewayService) ApplyOpenAIPromptCacheCreationOptimizationBody(account *Account, upstreamModel string, body []byte) ([]byte, openAIPromptCacheCreationOptimizationResult, error) {
	if !openAIPromptCacheCreationOptimizationRuntimeApplicable(account, upstreamModel, body, nil) {
		return body, openAIPromptCacheCreationOptimizationResult{}, nil
	}
	if !s.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account) {
		return body, openAIPromptCacheCreationOptimizationResult{}, nil
	}
	return applyOpenAIPromptCacheCreationOptimizationBody(account, upstreamModel, body)
}

func (s *OpenAIGatewayService) ApplyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(account *Account, upstreamModel string, body []byte, explicitImageIntent bool) ([]byte, openAIPromptCacheCreationOptimizationResult, error) {
	if !openAIPromptCacheCreationOptimizationRuntimeApplicable(account, upstreamModel, body, &explicitImageIntent) {
		return body, openAIPromptCacheCreationOptimizationResult{}, nil
	}
	if !s.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account) {
		return body, openAIPromptCacheCreationOptimizationResult{}, nil
	}
	return applyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(account, upstreamModel, body, explicitImageIntent)
}

func openAIPromptCacheCreationOptimizationRuntimeApplicable(account *Account, upstreamModel string, body []byte, explicitImageIntent *bool) bool {
	if account == nil || !account.IsOpenAIPromptCacheCreationOptimizationEnabled() || !isOpenAIGPT56Model(upstreamModel) || isOpenAIImageGenerationModel(upstreamModel) {
		return false
	}
	if explicitImageIntent != nil {
		return !*explicitImageIntent
	}
	return !IsExplicitImageGenerationIntent(openAIResponsesEndpoint, upstreamModel, body)
}

// RecordOpenAIPromptCacheCreationOptimizationUnsupported shares a 10-minute
// compatibility backoff between replicas after an upstream rejects the policy.
func (s *OpenAIGatewayService) RecordOpenAIPromptCacheCreationOptimizationUnsupported(account *Account) {
	if s == nil || account == nil || account.ID <= 0 {
		return
	}
	until := time.Now().Add(openAIPromptCacheCreationUnsupportedDisableTTL)
	s.openaiPromptCacheCreationDisabledUntil.Store(account.ID, until)
	if s.openaiAccountHealthRedis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	pipe := s.openaiAccountHealthRedis.TxPipeline()
	key := openAIPromptCacheCreationDisabledRedisPrefix + strconv.FormatInt(account.ID, 10)
	pipe.HSet(ctx, key, "disabled_until", until.Unix())
	pipe.Expire(ctx, key, openAIPromptCacheCreationUnsupportedDisableTTL)
	_, _ = pipe.Exec(ctx)
	cancel()
}
