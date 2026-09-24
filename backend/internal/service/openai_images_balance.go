package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const OpenAIImagesInsufficientBalanceReason GatewayFailureReason = "openai_images_insufficient_balance"

func isOpenAIImagesInsufficientBalance(body []byte) bool {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return false
	}
	var visit func(any, int) bool
	visit = func(v any, depth int) bool {
		if depth > 6 {
			return false
		}
		switch x := v.(type) {
		case map[string]any:
			for key, child := range x {
				key = strings.ToLower(key)
				if key == "code" || key == "error_key" || key == "errorkey" {
					if text, ok := child.(string); ok && strings.EqualFold(strings.TrimSpace(text), "insufficient_balance") {
						return true
					}
				}
				switch key {
				case "error", "errors", "cause", "detail", "details", "inner", "inner_error", "response":
					if visit(child, depth+1) {
						return true
					}
				}
			}
		case []any:
			for _, child := range x {
				if visit(child, depth+1) {
					return true
				}
			}
		}
		return false
	}
	return visit(value, 0)
}
func (s *OpenAIGatewayService) imageBalanceFailover(ctx context.Context, account *Account, resp *http.Response, body []byte) *UpstreamFailoverError {
	if s.accountRepo != nil {
		// Scope cooldown to image generation; ordinary text traffic stays eligible.
		if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, openAIImageGenerationRateLimitKey, time.Now().Add(5*time.Minute), "openai_images_insufficient_balance"); err != nil {
			slog.Warn("image_balance_cooldown_failed", "account_id", account.ID, "error", err)
		}
	}
	return &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: body, ResponseHeaders: resp.Header.Clone(), Reason: OpenAIImagesInsufficientBalanceReason, ClientStatusCode: http.StatusPaymentRequired, ClientMessage: "Upstream image account has insufficient balance", NextAccountAction: NextAccountRetry}
}
