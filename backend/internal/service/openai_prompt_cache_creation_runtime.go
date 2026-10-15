package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	openAIPromptCacheCreationUnsupportedDisableTTL = 10 * time.Minute
	openAIPromptCacheCreationRemotePollInterval    = time.Second
	openAIPromptCacheCreationDisabledRedisPrefix   = "openai:prompt_cache_creation_disabled:v1:"
	openAIPromptCacheCreationMaxRuntimeStates      = 4096
)

type openAIPromptCacheCreationRuntimeState struct {
	disabledUntil time.Time
	checkedAt     time.Time
}

// IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled applies the shared
// compatibility backoff without changing the account's configured setting.
func (s *OpenAIGatewayService) IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account *Account, scope ...string) bool {
	if account == nil || !account.IsOpenAIPromptCacheCreationOptimizationEnabled() {
		return false
	}
	if s == nil {
		return true
	}
	key := openAIPromptCacheCreationRuntimeKey(account.ID, scope)
	now := time.Now()
	s.openaiPromptCacheCreationRuntimeMu.Lock()
	state, exists := s.openaiPromptCacheCreationRuntime[key]
	if now.Before(state.disabledUntil) {
		s.openaiPromptCacheCreationRuntimeMu.Unlock()
		return false
	}
	if !exists && len(s.openaiPromptCacheCreationRuntime) >= openAIPromptCacheCreationMaxRuntimeStates && !s.storePromptCacheCreationRuntimeStateLocked(key, state, now) {
		s.openaiPromptCacheCreationRuntimeMu.Unlock()
		return false
	}
	if s.openaiAccountHealthRedis == nil || now.Sub(state.checkedAt) < openAIPromptCacheCreationRemotePollInterval {
		s.openaiPromptCacheCreationRuntimeMu.Unlock()
		return true
	}
	state.checkedAt = now
	// Reserve the poll before leaving the lock so concurrent requests do not
	// all hit Redis. Arbitrary model aliases cannot grow this cache unbounded.
	stored := s.storePromptCacheCreationRuntimeStateLocked(key, state, now)
	s.openaiPromptCacheCreationRuntimeMu.Unlock()
	if !stored {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	raw, err := s.openaiAccountHealthRedis.HGet(ctx, openAIPromptCacheCreationDisabledRedisPrefix+fmt.Sprint(key), "disabled_until").Result()
	cancel()
	if err != nil {
		return true
	}
	untilUnix, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || untilUnix <= now.Unix() {
		return true
	}
	until := time.Unix(untilUnix, 0)
	s.storePromptCacheCreationDisabledUntil(key, until)
	return false
}

// Caller holds runtimeMu. Preserve active backoffs when pruning remote polls.
func (s *OpenAIGatewayService) storePromptCacheCreationRuntimeStateLocked(key any, state openAIPromptCacheCreationRuntimeState, now time.Time) bool {
	if s.openaiPromptCacheCreationRuntime == nil {
		s.openaiPromptCacheCreationRuntime = make(map[any]openAIPromptCacheCreationRuntimeState)
	}
	if _, exists := s.openaiPromptCacheCreationRuntime[key]; !exists && len(s.openaiPromptCacheCreationRuntime) >= openAIPromptCacheCreationMaxRuntimeStates {
		if !now.Before(s.openaiPromptCacheCreationRuntimeCleanupAt) {
			s.openaiPromptCacheCreationRuntimeCleanupAt = now.Add(openAIPromptCacheCreationRemotePollInterval)
			for candidate, cached := range s.openaiPromptCacheCreationRuntime {
				if !now.Before(cached.disabledUntil) && now.Sub(cached.checkedAt) >= openAIPromptCacheCreationRemotePollInterval {
					delete(s.openaiPromptCacheCreationRuntime, candidate)
				}
			}
		}
		if len(s.openaiPromptCacheCreationRuntime) >= openAIPromptCacheCreationMaxRuntimeStates {
			return false
		}
	}
	s.openaiPromptCacheCreationRuntime[key] = state
	return true
}

func (s *OpenAIGatewayService) storePromptCacheCreationDisabledUntil(key any, until time.Time) {
	s.openaiPromptCacheCreationRuntimeMu.Lock()
	defer s.openaiPromptCacheCreationRuntimeMu.Unlock()
	state := s.openaiPromptCacheCreationRuntime[key]
	if until.After(state.disabledUntil) {
		state.disabledUntil = until
	}
	s.storePromptCacheCreationRuntimeStateLocked(key, state, time.Now())
}

// ApplyOpenAIPromptCacheCreationOptimizationBody is the service-aware wrapper
// used by request paths. The pure helper remains available for unit tests.
func (s *OpenAIGatewayService) ApplyOpenAIPromptCacheCreationOptimizationBody(account *Account, upstreamModel string, body []byte) ([]byte, openAIPromptCacheCreationOptimizationResult, error) {
	if !openAIPromptCacheCreationOptimizationRuntimeApplicable(account, upstreamModel, body, nil) {
		return body, openAIPromptCacheCreationOptimizationResult{}, nil
	}
	if !s.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account, OpenAIPromptCacheCreationOptimizationScope(upstreamModel, body)) {
		return body, openAIPromptCacheCreationOptimizationResult{}, nil
	}
	return applyOpenAIPromptCacheCreationOptimizationBody(account, upstreamModel, body)
}

func (s *OpenAIGatewayService) ApplyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(account *Account, upstreamModel string, body []byte, explicitImageIntent bool) ([]byte, openAIPromptCacheCreationOptimizationResult, error) {
	if !openAIPromptCacheCreationOptimizationRuntimeApplicable(account, upstreamModel, body, &explicitImageIntent) {
		return body, openAIPromptCacheCreationOptimizationResult{}, nil
	}
	if !s.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account, OpenAIPromptCacheCreationOptimizationScope(upstreamModel, body)) {
		return body, openAIPromptCacheCreationOptimizationResult{}, nil
	}
	return applyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(account, upstreamModel, body, explicitImageIntent)
}

func openAIPromptCacheCreationOptimizationRuntimeApplicable(account *Account, upstreamModel string, body []byte, explicitImageIntent *bool) bool {
	if account == nil || !account.IsOpenAIPromptCacheCreationOptimizationEnabled() || !isOpenAICacheCreationTextModel(upstreamModel) || isOpenAIImageGenerationModel(upstreamModel) {
		return false
	}
	if explicitImageIntent != nil {
		return !*explicitImageIntent
	}
	return !IsExplicitImageGenerationIntent(openAIResponsesEndpoint, upstreamModel, body)
}

// RecordOpenAIPromptCacheCreationOptimizationUnsupported shares a 10-minute
// compatibility backoff between replicas after an upstream rejects the policy.
func (s *OpenAIGatewayService) RecordOpenAIPromptCacheCreationOptimizationUnsupported(account *Account, scope ...string) {
	if s == nil || account == nil || account.ID <= 0 {
		return
	}
	key := openAIPromptCacheCreationRuntimeKey(account.ID, scope)
	until := time.Now().Add(openAIPromptCacheCreationUnsupportedDisableTTL)
	s.storePromptCacheCreationDisabledUntil(key, until)
	if s.openaiAccountHealthRedis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	pipe := s.openaiAccountHealthRedis.TxPipeline()
	redisKey := openAIPromptCacheCreationDisabledRedisPrefix + fmt.Sprint(key)
	pipe.HSet(ctx, redisKey, "disabled_until", until.Unix())
	pipe.Expire(ctx, redisKey, openAIPromptCacheCreationUnsupportedDisableTTL)
	_, _ = pipe.Exec(ctx)
	cancel()
}

// Scope separates capability backoff by the actual upstream model and protocol.
func OpenAIPromptCacheCreationOptimizationScope(model string, body []byte) string {
	protocol := "responses"
	if gjson.GetBytes(body, "type").String() == "response.create" {
		protocol = "responses_ws"
	} else if gjson.GetBytes(body, "messages").Exists() {
		protocol = "chat_completions"
	}
	return openAIPromptCacheCreationScope(model, protocol)
}
func openAIPromptCacheCreationOptimizationMapScope(model string, request map[string]any) string {
	protocol := "responses"
	if request["type"] == "response.create" {
		protocol = "responses_ws"
	} else if _, ok := request["messages"]; ok {
		protocol = "chat_completions"
	}
	return openAIPromptCacheCreationScope(model, protocol)
}
func openAIPromptCacheCreationScope(model, protocol string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(model)+"\x00"+protocol)))
}
func openAIPromptCacheCreationRuntimeKey(accountID int64, scopes []string) any {
	if len(scopes) == 0 || scopes[0] == "" {
		return accountID
	}
	return strconv.FormatInt(accountID, 10) + ":" + scopes[0]
}
