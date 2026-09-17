package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAINativeCompactionV2Key = "openai_native_compaction_v2"
const openAIRemoteCompactionV2Feature = "remote_compaction_v2"

type openAINativeCompactionV2ContextKey struct{}

// MarkOpenAINativeCompactionV2 records only the request classification, never payload data.
func MarkOpenAINativeCompactionV2(c *gin.Context) {
	if c != nil {
		c.Set(openAINativeCompactionV2Key, true)
	}
}

func IsOpenAINativeCompactionV2(c *gin.Context) bool {
	return c != nil && c.GetBool(openAINativeCompactionV2Key)
}

func WithOpenAINativeCompactionV2(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAINativeCompactionV2ContextKey{}, true)
}

func IsOpenAINativeCompactionV2Context(ctx context.Context) bool {
	return ctx != nil && ctx.Value(openAINativeCompactionV2ContextKey{}) == true
}

func ensureOpenAIRemoteCompactionV2BetaFeature(h http.Header) {
	if h == nil {
		return
	}
	tokens := make([]string, 0, 4)
	for _, value := range h.Values("x-codex-beta-features") {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			if token == openAIRemoteCompactionV2Feature {
				return
			}
			tokens = append(tokens, token)
		}
	}
	tokens = append(tokens, openAIRemoteCompactionV2Feature)
	h.Set("x-codex-beta-features", strings.Join(tokens, ","))
}

func hasOpenAICodexBetaFeaturesHeader(h http.Header) bool {
	if h == nil {
		return false
	}
	for _, value := range h.Values("x-codex-beta-features") {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// Native v2 turns always advertise the feature. OAuth otherwise mirrors the
// session-level header emitted by current Codex clients.
func applyOpenAICodexBetaFeatures(c *gin.Context, account *Account, h http.Header) {
	if h == nil {
		return
	}
	if IsOpenAINativeCompactionV2(c) {
		ensureOpenAIRemoteCompactionV2BetaFeature(h)
		return
	}
	if account == nil || !account.IsOpenAIOAuthLike() || hasOpenAICodexBetaFeaturesHeader(h) {
		return
	}
	h.Set("x-codex-beta-features", openAIRemoteCompactionV2Feature)
}

// HasCompactionTriggerInInput detects Codex remote compact v2 requests that
// carry the compact signal in a normal /v1/responses body.
func HasCompactionTriggerInInput(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "compaction_trigger" {
			found = true
			return false
		}
		return true
	})
	return found
}
