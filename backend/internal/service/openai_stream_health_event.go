package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// OpenAIStreamFailureCountsForAccount distinguishes server faults from
// compatibility refusals that can omit an HTTP status/type. They are not evidence
// of a broken account, even when the generic stream status falls back to 502.
func OpenAIStreamFailureCountsForAccount(payload []byte, message string) bool {
	if message == "" {
		message = extractOpenAISSEErrorMessage(payload)
	}
	if strings.Contains(strings.ToLower(message), "cannot serve requested model") {
		return false
	}
	if !openAIStreamFailedEventShouldFailover(payload, message) || openAIStreamFailedEventSemanticStatus(payload, message) < 500 {
		return false
	}
	if isOpenAIPromptCacheCreationOptimizationUnsupportedError(http.StatusBadRequest, message, payload) ||
		isOpenAIPromptCacheBoostUnsupportedError(http.StatusBadRequest, message, payload) ||
		isOpenAIPoolModelRoutingError(http.StatusBadGateway, message, payload) ||
		isOpenAIPoolAccountModelCapabilityError(http.StatusBadGateway, message, payload) {
		return false
	}
	errorPayload := payload
	if response := gjson.GetBytes(payload, "response"); response.IsObject() {
		errorPayload = []byte(response.Raw)
	}
	code, errorType, errorMessage := parseOpenAIWSErrorEventFields(errorPayload)
	combined := strings.ToLower(strings.Join([]string{code, errorType, errorMessage, message}, " "))
	if containsAnySubstring(combined, "model_not_supported", "unsupported_parameter", "unknown_parameter", "unsupported parameter", "unknown parameter", "unrecognized parameter") {
		return false
	}
	reason, _ := classifyOpenAIWSErrorEventFromRaw(code, errorType, errorMessage)
	switch reason {
	case "previous_response_not_found", "invalid_encrypted_content", "upgrade_required", "ws_unsupported", "ws_connection_limit_reached":
		return false
	}
	return true
}

// Stream health receipts are separate from usage settlement identities: a WS
// session may contain many turns but still has exactly one billing settlement.
// A shared receipt makes WAL replay and callbacks landing on another replica
// idempotent. Redis outages are retried by the Edge WAL instead of falling back
// to conflicting process-local claims.
func (s *OpenAIGatewayService) RecordOpenAIStreamHealthEvent(ctx context.Context, eventID string, account *Account, model, class string, success bool, firstTokenMS *int) error {
	return s.recordOpenAIStreamHealthEvent(ctx, eventID, account, model, class, success, firstTokenMS, 0, "")
}

// RecordOpenAIStreamHealthEventWithContext is the Edge WS variant. A health
// receipt is emitted before the client can close the socket, so it also carries
// the semantic upstream status/message needed to preserve the normal committed
// response side effects (rate-limit, pool recovery and cache compatibility).
func (s *OpenAIGatewayService) RecordOpenAIStreamHealthEventWithContext(ctx context.Context, eventID string, account *Account, model, class string, success bool, firstTokenMS *int, statusCode int, errorMessage string) error {
	return s.recordOpenAIStreamHealthEvent(ctx, eventID, account, model, class, success, firstTokenMS, statusCode, errorMessage)
}

func (s *OpenAIGatewayService) recordOpenAIStreamHealthEvent(ctx context.Context, eventID string, account *Account, model, class string, success bool, firstTokenMS *int, statusCode int, errorMessage string) error {
	if s == nil || account == nil || eventID == "" {
		return fmt.Errorf("invalid stream health event")
	}
	receipt := fmt.Sprintf("openai:stream-health-event:%x", sha256.Sum256([]byte(eventID)))
	if s.openaiAccountHealthRedis != nil {
		claimed, err := s.openaiAccountHealthRedis.SetNX(ctx, receipt, "1", 24*time.Hour).Result()
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
	} else {
		s.openaiStreamHealthMu.Lock()
		now := time.Now()
		if expiry, ok := s.openaiStreamHealthReceipts[receipt]; ok && now.Before(expiry) {
			s.openaiStreamHealthMu.Unlock()
			return nil
		}
		if s.openaiStreamHealthReceipts == nil {
			s.openaiStreamHealthReceipts = make(map[string]time.Time)
		}
		if len(s.openaiStreamHealthReceipts) >= 8192 {
			var oldestKey string
			var oldest time.Time
			for key, expiry := range s.openaiStreamHealthReceipts {
				if !now.Before(expiry) {
					delete(s.openaiStreamHealthReceipts, key)
					continue
				}
				if oldestKey == "" || expiry.Before(oldest) {
					oldestKey = key
					oldest = expiry
				}
			}
			if len(s.openaiStreamHealthReceipts) >= 8192 && oldestKey != "" {
				delete(s.openaiStreamHealthReceipts, oldestKey)
			}
		}
		s.openaiStreamHealthReceipts[receipt] = now.Add(24 * time.Hour)
		s.openaiStreamHealthMu.Unlock()
	}
	s.RecordOpenAIEdgeStreamOutcome(account, class, false, success, "")
	if !success {
		firstTokenMS = nil
		if statusCode < http.StatusBadRequest {
			statusCode = http.StatusBadGateway
		}
		message := sanitizeUpstreamErrorMessage(strings.TrimSpace(errorMessage))
		if message == "" {
			message = "Upstream request failed"
		}
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
		s.HandleOpenAIAccountUpstreamErrorAfterCommittedResponse(ctx, account, statusCode, nil, body, model)
		s.RecordOpenAIPromptCacheBoostUnsupportedAfterCommittedResponse(account, statusCode, message, body, true, true)
		s.RecordOpenAIPoolFailureAfterCommittedResponse(ctx, account, statusCode, body, model, message)
	}
	s.ReportOpenAIAccountScheduleResultForRequest(account, model, success, firstTokenMS)
	return nil
}
