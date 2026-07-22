package service

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const openAIPromptCacheBoostUnsupportedDisableTTL = 10 * time.Minute
const openAIPromptCacheBoostRemotePollInterval = time.Second
const openAIPromptCacheBoostDisabledRedisPrefix = "openai:prompt_cache_boost_disabled:v1:"

type openAIPromptCacheBoostDisabledState struct {
	KeyUntil       time.Time
	RetentionUntil time.Time
}

func (s *OpenAIGatewayService) isOpenAIPromptCacheBoostRuntimeEnabled(account *Account) bool {
	if account == nil {
		return false
	}
	if !account.IsOpenAIPromptCacheBoostEnabled() {
		return false
	}
	return s.isOpenAIPromptCacheBoostKeyRuntimeEnabled(account) ||
		s.isOpenAIPromptCacheBoostRetentionRuntimeEnabled(account)
}

func (s *OpenAIGatewayService) isOpenAIPromptCacheBoostKeyRuntimeEnabled(account *Account) bool {
	return s.isOpenAIPromptCacheBoostFeatureRuntimeEnabled(account, "key")
}

func (s *OpenAIGatewayService) isOpenAIPromptCacheBoostAffinityAccountUsable(account *Account) bool {
	if account == nil || !account.IsOpenAI() || !account.IsSchedulable() {
		return false
	}
	return s.isOpenAIPromptCacheBoostKeyRuntimeEnabled(account) &&
		!s.isOpenAIPoolAccountSoftCooling(account) &&
		!s.isOpenAIAccountRuntimeBlocked(account)
}

func (s *OpenAIGatewayService) isOpenAIPromptCacheBoostAffinityHashUsableForAccount(sessionHash string, account *Account) bool {
	if !s.isOpenAIPromptCacheBoostAffinityAccountUsable(account) {
		return false
	}
	if IsOpenAIPromptCacheBoostUpstreamAffinitySessionHash(sessionHash) {
		return account.IsOpenAIPromptCacheBoostUpstreamHitPriorityEnabled()
	}
	if IsOpenAIPromptCacheBoostOptimizedAffinitySessionHash(sessionHash) {
		return account.IsOpenAIPromptCacheKeyOptimizationEnabled()
	}
	if IsOpenAIPromptCacheBoostAggressiveAffinitySessionHash(sessionHash) {
		return account.IsOpenAIPromptCacheBoostAggressive()
	}
	return true
}

func (s *OpenAIGatewayService) isOpenAIPromptCacheBoostAffinityAccountBindable(ctx context.Context, sessionHash string, accountID int64) bool {
	if s == nil || accountID <= 0 || (s.schedulerSnapshot == nil && s.accountRepo == nil) {
		return false
	}
	account, err := s.getSchedulableAccount(ctx, accountID)
	if err != nil {
		return false
	}
	return s.isOpenAIPromptCacheBoostAffinityHashUsableForAccount(sessionHash, account)
}

// NormalizeOpenAIPromptCacheBoostAffinitySessionHash keeps prompt-cache affinity
// only on OpenAI text-pool accounts that explicitly enabled it.
func (s *OpenAIGatewayService) NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash string, account *Account) string {
	sessionHash = strings.TrimSpace(sessionHash)
	if !IsOpenAIPromptCacheBoostAffinitySessionHash(sessionHash) {
		return sessionHash
	}
	if s.isOpenAIPromptCacheBoostAffinityHashUsableForAccount(sessionHash, account) {
		return sessionHash
	}
	return ""
}

func (s *OpenAIGatewayService) isOpenAIPromptCacheBoostRetentionRuntimeEnabled(account *Account) bool {
	return s.isOpenAIPromptCacheBoostFeatureRuntimeEnabled(account, "retention")
}

func (s *OpenAIGatewayService) isOpenAIPromptCacheBoostFeatureRuntimeEnabled(account *Account, feature string) bool {
	if account == nil {
		return false
	}
	if !account.IsOpenAIPromptCacheBoostEnabled() {
		return false
	}
	if s == nil {
		return true
	}
	state := s.openAIPromptCacheBoostDisabledState(account.ID)
	now := time.Now()
	switch feature {
	case "key":
		return state.KeyUntil.IsZero() || !now.Before(state.KeyUntil)
	case "retention":
		return state.RetentionUntil.IsZero() || !now.Before(state.RetentionUntil)
	default:
		return true
	}
}

func (s *OpenAIGatewayService) openAIPromptCacheBoostDisabledState(accountID int64) openAIPromptCacheBoostDisabledState {
	if s == nil || accountID == 0 {
		return openAIPromptCacheBoostDisabledState{}
	}
	raw, ok := s.openaiPromptCacheBoostDisabledUntil.Load(accountID)
	state := openAIPromptCacheBoostDisabledState{}
	if ok {
		var stateOK bool
		state, stateOK = raw.(openAIPromptCacheBoostDisabledState)
		if !stateOK {
			s.openaiPromptCacheBoostDisabledUntil.Delete(accountID)
		}
	}
	now := time.Now()
	if !state.KeyUntil.IsZero() && !now.Before(state.KeyUntil) {
		state.KeyUntil = time.Time{}
	}
	if !state.RetentionUntil.IsZero() && !now.Before(state.RetentionUntil) {
		state.RetentionUntil = time.Time{}
	}
	if state.KeyUntil.IsZero() && state.RetentionUntil.IsZero() {
		s.openaiPromptCacheBoostDisabledUntil.Delete(accountID)
		state = openAIPromptCacheBoostDisabledState{}
	}
	if s.openaiAccountHealthRedis != nil && s.shouldPollPromptCacheBoostDisabledState(accountID, now) {
		remote := s.loadOpenAIPromptCacheBoostDisabledState(accountID)
		s.openaiPromptCacheBoostRemoteCheckedAt.Store(accountID, now)
		if remote.KeyUntil.After(state.KeyUntil) {
			state.KeyUntil = remote.KeyUntil
		}
		if remote.RetentionUntil.After(state.RetentionUntil) {
			state.RetentionUntil = remote.RetentionUntil
		}
	}
	if state.KeyUntil.IsZero() && state.RetentionUntil.IsZero() {
		s.openaiPromptCacheBoostDisabledUntil.Delete(accountID)
	} else {
		s.openaiPromptCacheBoostDisabledUntil.Store(accountID, state)
	}
	return state
}

func (s *OpenAIGatewayService) shouldPollPromptCacheBoostDisabledState(accountID int64, now time.Time) bool {
	last, ok := s.openaiPromptCacheBoostRemoteCheckedAt.Load(accountID)
	if !ok {
		return true
	}
	checked, ok := last.(time.Time)
	return !ok || now.Sub(checked) >= openAIPromptCacheBoostRemotePollInterval
}

func (s *OpenAIGatewayService) loadOpenAIPromptCacheBoostDisabledState(accountID int64) openAIPromptCacheBoostDisabledState {
	if s == nil || s.openaiAccountHealthRedis == nil || accountID <= 0 {
		return openAIPromptCacheBoostDisabledState{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	values, err := s.openaiAccountHealthRedis.HGetAll(ctx, openAIPromptCacheBoostDisabledRedisPrefix+strconv.FormatInt(accountID, 10)).Result()
	if err != nil {
		return openAIPromptCacheBoostDisabledState{}
	}
	return parseOpenAIPromptCacheBoostDisabledState(values)
}

func parseOpenAIPromptCacheBoostDisabledState(values map[string]string) openAIPromptCacheBoostDisabledState {
	var state openAIPromptCacheBoostDisabledState
	if raw := strings.TrimSpace(values["key"]); raw != "" {
		if unix, err := strconv.ParseInt(raw, 10, 64); err == nil {
			state.KeyUntil = time.Unix(unix, 0)
		}
	}
	if raw := strings.TrimSpace(values["retention"]); raw != "" {
		if unix, err := strconv.ParseInt(raw, 10, 64); err == nil {
			state.RetentionUntil = time.Unix(unix, 0)
		}
	}
	return state
}

func (s *OpenAIGatewayService) temporarilyDisableOpenAIPromptCacheBoost(account *Account, disableKey bool, disableRetention bool) {
	if s == nil || account == nil || account.ID == 0 {
		return
	}
	state := s.openAIPromptCacheBoostDisabledState(account.ID)
	until := time.Now().Add(openAIPromptCacheBoostUnsupportedDisableTTL)
	if s.openaiAccountHealthRedis != nil {
		remote := s.loadOpenAIPromptCacheBoostDisabledState(account.ID)
		if remote.KeyUntil.After(state.KeyUntil) {
			state.KeyUntil = remote.KeyUntil
		}
		if remote.RetentionUntil.After(state.RetentionUntil) {
			state.RetentionUntil = remote.RetentionUntil
		}
	}
	if disableKey {
		state.KeyUntil = until
	}
	if disableRetention {
		state.RetentionUntil = until
	}
	if !state.KeyUntil.IsZero() || !state.RetentionUntil.IsZero() {
		s.openaiPromptCacheBoostDisabledUntil.Store(account.ID, state)
		if s.openaiAccountHealthRedis != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			pipe := s.openaiAccountHealthRedis.TxPipeline()
			key := openAIPromptCacheBoostDisabledRedisPrefix + strconv.FormatInt(account.ID, 10)
			if disableKey {
				pipe.HSet(ctx, key, "key", state.KeyUntil.Unix())
			}
			if disableRetention {
				pipe.HSet(ctx, key, "retention", state.RetentionUntil.Unix())
			}
			pipe.Expire(ctx, key, openAIPromptCacheBoostUnsupportedDisableTTL)
			_, _ = pipe.Exec(ctx)
			cancel()
		}
	}
}

// RecordOpenAIPromptCacheBoostUnsupportedAfterCommittedResponse preserves the
// unsupported-field backoff when a streaming response has already been committed
// and the current request can no longer be retried without those fields.
func (s *OpenAIGatewayService) RecordOpenAIPromptCacheBoostUnsupportedAfterCommittedResponse(
	account *Account,
	statusCode int,
	upstreamMsg string,
	upstreamBody []byte,
	keyInjected bool,
	retentionInjected bool,
) {
	if account == nil || !account.IsOpenAIPromptCacheBoostEnabled() {
		return
	}
	if !keyInjected && !retentionInjected {
		return
	}
	keyUnsupported, retentionUnsupported := openAIPromptCacheBoostUnsupportedFields(statusCode, upstreamMsg, upstreamBody)
	disableKey := keyInjected && keyUnsupported
	disableRetention := retentionInjected && retentionUnsupported
	if disableKey || disableRetention {
		s.temporarilyDisableOpenAIPromptCacheBoost(account, disableKey, disableRetention)
	}
}

func isOpenAIPromptCacheBoostUnsupportedError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	keyUnsupported, retentionUnsupported := openAIPromptCacheBoostUnsupportedFields(statusCode, upstreamMsg, upstreamBody)
	return keyUnsupported || retentionUnsupported
}

func openAIPromptCacheBoostUnsupportedFields(statusCode int, upstreamMsg string, upstreamBody []byte) (keyUnsupported bool, retentionUnsupported bool) {
	if statusCode < 400 || statusCode >= 500 {
		return false, false
	}
	combined := openAIPoolCombinedErrorText(upstreamMsg, upstreamBody)
	if combined == "" {
		return false, false
	}
	if !containsAnySubstring(combined,
		"unsupported parameter",
		"unknown parameter",
		"unrecognized parameter",
		"unknown field",
		"extra inputs are not permitted",
		"not supported",
		"unsupported",
	) {
		return false, false
	}
	keyUnsupported = strings.Contains(combined, "prompt_cache_key") ||
		strings.Contains(combined, "prompt cache key")
	retentionUnsupported = strings.Contains(combined, "prompt_cache_retention") ||
		strings.Contains(combined, "prompt cache retention")
	return keyUnsupported, retentionUnsupported
}

func stripOpenAIPromptCacheBoostFields(body []byte, stripKey bool, stripRetention bool) ([]byte, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false
	}
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, false
	}
	changed := stripOpenAIPromptCacheBoostFieldsMap(reqBody, stripKey, stripRetention)
	if !changed {
		return body, false
	}
	updated, err := json.Marshal(reqBody)
	if err != nil {
		return body, false
	}
	return updated, true
}

func stripOpenAIPromptCacheBoostFieldsMap(reqBody map[string]any, stripKey bool, stripRetention bool) bool {
	if reqBody == nil {
		return false
	}
	changed := false
	if stripKey {
		if _, ok := reqBody["prompt_cache_key"]; ok {
			delete(reqBody, "prompt_cache_key")
			changed = true
		}
	}
	if stripRetention {
		if _, ok := reqBody["prompt_cache_retention"]; ok {
			delete(reqBody, "prompt_cache_retention")
			changed = true
		}
	}
	return changed
}
