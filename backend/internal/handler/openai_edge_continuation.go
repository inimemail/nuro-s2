package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/runtimeops"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	openAIEdgeContinuationPrefix     = "sub2api:edge_cont:"
	openAIEdgeContinuationTTL        = 10 * time.Second
	openAIEdgeContinuationMaxEntries = 8192
)

type openAIEdgeContinuationMemory struct {
	expiresAt time.Time
	body      []byte
}

// edgeRetryContinuation contains only failover state. Request bodies, cache
// keys and cache policy fields remain owned by the existing Edge lease/handler
// pipeline and are never serialized here.
type edgeRetryContinuation struct {
	Version                 int                      `json:"version"`
	EdgeRequestID           string                   `json:"edge_request_id"`
	StartedAtUnixMS         int64                    `json:"started_at_unix_ms,omitempty"`
	DeadlineUnixMS          int64                    `json:"deadline_unix_ms,omitempty"`
	SameAccountRetries      map[int64]int            `json:"same_account_retries,omitempty"`
	SameAccountRuleRetries  map[int64]map[string]int `json:"same_account_rule_retries,omitempty"`
	FailedAccountIDs        []int64                  `json:"failed_account_ids,omitempty"`
	SwitchCount             int                      `json:"switch_count,omitempty"`
	ExecutionUnknownRetries int                      `json:"execution_unknown_retries,omitempty"`
	LastAccountID           int64                    `json:"last_account_id,omitempty"`
	BudgetExhausted         bool                     `json:"budget_exhausted,omitempty"`
}

func (h *OpenAIGatewayHandler) attachOpenAIEdgeContinuation(ctx context.Context, leaseID string, decision *service.OpenAIEdgeRetryDecision) {
	if h == nil || decision == nil || decision.ContinuationToken != "" || decision.Action == service.OpenAIEdgeActionRespondError {
		return
	}
	lease := h.getOpenAIEdgeLease(leaseID)
	if lease == nil {
		return
	}
	state := h.snapshotOpenAIEdgeContinuation(lease)
	if state.SwitchCount <= 0 && len(state.SameAccountRetries) == 0 && state.ExecutionUnknownRetries <= 0 {
		return
	}
	token := uuid.NewString()
	if h.saveOpenAIEdgeContinuation(ctx, token, state) {
		decision.ContinuationToken = token
	}
}

func cloneRetryCounts(src map[int64]int) map[int64]int {
	if len(src) == 0 {
		return make(map[int64]int)
	}
	dst := make(map[int64]int, len(src))
	for id, count := range src {
		if id > 0 && count > 0 {
			dst[id] = count
		}
	}
	return dst
}

func cloneRetryRuleCounts(src sameAccountRetryRuleCounts) map[int64]map[string]int {
	if len(src) == 0 {
		return make(map[int64]map[string]int)
	}
	dst := make(map[int64]map[string]int, len(src))
	for id, rules := range src {
		if id <= 0 || len(rules) == 0 {
			continue
		}
		copyRules := make(map[string]int, len(rules))
		for key, count := range rules {
			if strings.TrimSpace(key) != "" && count > 0 {
				copyRules[key] = count
			}
		}
		if len(copyRules) > 0 {
			dst[id] = copyRules
		}
	}
	return dst
}

func (h *OpenAIGatewayHandler) snapshotOpenAIEdgeContinuation(lease *openAIEdgeLease) edgeRetryContinuation {
	state := edgeRetryContinuation{Version: 1, SameAccountRetries: make(map[int64]int)}
	if lease == nil {
		return state
	}
	// Retry decisions and lease lifecycle transitions use the lock order
	// retryMu -> mu. Take both locks here so continuation snapshots cannot
	// race with an in-flight retry decision updating the same fields.
	lease.retryMu.Lock()
	defer lease.retryMu.Unlock()
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return h.snapshotOpenAIEdgeContinuationLocked(lease)
}

func (h *OpenAIGatewayHandler) snapshotOpenAIEdgeContinuationLocked(lease *openAIEdgeLease) edgeRetryContinuation {
	state := edgeRetryContinuation{Version: 1, SameAccountRetries: make(map[int64]int)}
	if lease == nil {
		return state
	}
	state.EdgeRequestID = lease.edgeRequestID
	state.SameAccountRetries = cloneRetryCounts(lease.sameAccountRetries)
	state.SameAccountRuleRetries = cloneRetryRuleCounts(lease.sameRuleRetries)
	state.SwitchCount = lease.switchCount
	state.ExecutionUnknownRetries = lease.executionUnknownRetries
	state.LastAccountID = 0
	if lease.account != nil {
		state.LastAccountID = lease.account.ID
	}
	for id := range lease.failedAccountIDs {
		if id > 0 {
			state.FailedAccountIDs = append(state.FailedAccountIDs, id)
		}
	}
	if startedAt := lease.sameAccountStarted[sharedRaceRetryStartedKey]; !startedAt.IsZero() {
		state.StartedAtUnixMS = startedAt.UnixMilli()
	}
	if deadline := lease.sameAccountStarted[sharedRaceRetryDeadlineKey]; !deadline.IsZero() {
		state.DeadlineUnixMS = deadline.UnixMilli()
		state.BudgetExhausted = !lease.sameAccountStarted[sharedRaceRetryExhaustedKey].IsZero() || !deadline.After(time.Now())
	} else if !lease.sameAccountStarted[sharedRaceRetryExhaustedKey].IsZero() {
		state.BudgetExhausted = true
	}
	return state
}

func (h *OpenAIGatewayHandler) saveOpenAIEdgeContinuation(ctx context.Context, token string, state edgeRetryContinuation) bool {
	token = strings.TrimSpace(token)
	if h == nil || token == "" {
		return false
	}
	if state.Version == 0 {
		state.Version = 1
	}
	body, err := json.Marshal(state)
	if err != nil {
		return false
	}
	if h.redisClient != nil {
		if err := h.redisClient.Set(ctx, openAIEdgeContinuationPrefix+token, body, openAIEdgeContinuationTTL).Err(); err == nil {
			runtimeops.ObserveEdgeContinuationCreated()
			return true
		} else {
			logger.FromContext(ctx).Warn(
				"openai_edge.continuation_redis_save_failed", zap.Error(err))
			// Redis is the configured shared source of truth. A write error can be
			// ambiguous (the server may have committed it), so never create a
			// second local representation of the same token.
			return false
		}
	}
	h.openAIEdgeContinuationMu.Lock()
	if h.openAIEdgeContinuations == nil {
		h.openAIEdgeContinuations = make(map[string]openAIEdgeContinuationMemory)
	}
	if len(h.openAIEdgeContinuations) >= openAIEdgeContinuationMaxEntries {
		now := time.Now()
		for key, memory := range h.openAIEdgeContinuations {
			if !memory.expiresAt.After(now) {
				delete(h.openAIEdgeContinuations, key)
			}
		}
		if len(h.openAIEdgeContinuations) >= openAIEdgeContinuationMaxEntries {
			for key := range h.openAIEdgeContinuations {
				delete(h.openAIEdgeContinuations, key)
				break
			}
		}
	}
	h.openAIEdgeContinuations[token] = openAIEdgeContinuationMemory{expiresAt: time.Now().Add(openAIEdgeContinuationTTL), body: body}
	h.openAIEdgeContinuationMu.Unlock()
	runtimeops.ObserveEdgeContinuationCreated()
	return true
}

func (h *OpenAIGatewayHandler) consumeOpenAIEdgeContinuation(ctx context.Context, token string) (*edgeRetryContinuation, bool) {
	token = strings.TrimSpace(token)
	if h == nil || token == "" {
		return nil, false
	}
	key := openAIEdgeContinuationPrefix + token
	if h.redisClient != nil {
		body, err := h.redisClient.GetDel(ctx, key).Bytes()
		if err == nil {
			var state edgeRetryContinuation
			if json.Unmarshal(body, &state) == nil && state.Version == 1 {
				runtimeops.ObserveEdgeContinuationConsumed()
				return &state, true
			}
			return nil, false
		} else if errors.Is(err, redis.Nil) {
			// Redis is the shared source of truth. Do not consult the local fallback
			// on a clean miss or a replay could succeed on another representation.
			return nil, false
		} else {
			logger.FromContext(ctx).Warn(
				"openai_edge.continuation_redis_consume_failed", zap.Error(err))
			// Do not consult local memory after a Redis error. Falling back could
			// replay a token that Redis committed before returning the error.
			return nil, false
		}
	}
	h.openAIEdgeContinuationMu.Lock()
	memory, ok := h.openAIEdgeContinuations[token]
	if ok {
		delete(h.openAIEdgeContinuations, token)
	}
	h.openAIEdgeContinuationMu.Unlock()
	if !ok || time.Now().After(memory.expiresAt) {
		if ok {
			runtimeops.ObserveEdgeContinuationExpired()
		}
		return nil, false
	}
	var state edgeRetryContinuation
	if json.Unmarshal(memory.body, &state) != nil || state.Version != 1 {
		return nil, false
	}
	runtimeops.ObserveEdgeContinuationConsumed()
	return &state, true
}

func edgeRetryContinuationFromContext(ctx context.Context) *edgeRetryContinuation {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(ctxkey.EdgeRetryContinuation).(*edgeRetryContinuation)
	return state
}

func restoreOpenAIExecutionUnknownSwitchFromEdge(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	state := edgeRetryContinuationFromContext(c.Request.Context())
	if state == nil || state.ExecutionUnknownRetries <= 0 {
		return
	}
	count := state.ExecutionUnknownRetries
	if count > openAIExecutionUnknownSwitchLimit {
		count = openAIExecutionUnknownSwitchLimit
	}
	c.Set(openAIExecutionUnknownSwitchKey, count)
}

func seedRetryStateFromEdgeContinuation(ctx context.Context, counts map[int64]int, starts map[int64]time.Time, failed map[int64]struct{}, ruleCounts ...sameAccountRetryRuleCounts) (switchCount int, restored bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	state := edgeRetryContinuationFromContext(ctx)
	if state == nil {
		// An authenticated Edge fallback that already retried but lost its
		// one-time continuation must not start a second race window in Go.
		if retryCount, ok := ctx.Value(ctxkey.EdgeRetryCount).(int64); ok && retryCount > 0 {
			starts[sharedRaceRetryDeadlineKey] = time.Now().Add(-time.Millisecond)
		}
		return 0, false
	}
	for id, count := range state.SameAccountRetries {
		if id > 0 && count > 0 {
			counts[id] = count
		}
	}
	if len(ruleCounts) > 0 && ruleCounts[0] != nil {
		for id, rules := range state.SameAccountRuleRetries {
			if id <= 0 || len(rules) == 0 {
				continue
			}
			copyRules := make(map[string]int, len(rules))
			for key, count := range rules {
				if strings.TrimSpace(key) != "" && count > 0 {
					copyRules[key] = count
				}
			}
			if len(copyRules) > 0 {
				ruleCounts[0][id] = copyRules
			}
		}
	}
	for _, id := range state.FailedAccountIDs {
		if id > 0 {
			failed[id] = struct{}{}
		}
	}
	if state.StartedAtUnixMS > 0 {
		starts[sharedRaceRetryStartedKey] = time.UnixMilli(state.StartedAtUnixMS)
	}
	if state.DeadlineUnixMS > 0 {
		starts[sharedRaceRetryDeadlineKey] = time.UnixMilli(state.DeadlineUnixMS)
	} else if len(state.SameAccountRetries) > 0 {
		// Older/partial Edge state must fail closed: without the original
		// deadline Go cannot safely create a fresh race window.
		starts[sharedRaceRetryDeadlineKey] = time.Now().Add(-time.Millisecond)
	}
	if state.BudgetExhausted && state.DeadlineUnixMS == 0 {
		starts[sharedRaceRetryDeadlineKey] = time.Now().Add(-time.Millisecond)
	}
	if state.BudgetExhausted {
		starts[sharedRaceRetryExhaustedKey] = time.Now()
	}
	return state.SwitchCount, true
}
