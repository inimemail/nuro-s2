package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIWSFallbackReasonInvalidEncryptedContent = "invalid_encrypted_content"
const openAIWSIngressSessionHashContextKey = "openai_ws_ingress_session_hash"

func openAIEncryptedContentDigest(encrypted string) string {
	sum := sha256.Sum256([]byte(encrypted))
	return hex.EncodeToString(sum[:])
}

func openAIEncryptedLineageItemType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "reasoning", "compaction", "compaction_summary":
		return true
	default:
		return false
	}
}

func collectOpenAIEncryptedContentDigestsRaw(payload []byte) []string {
	if len(payload) == 0 {
		return nil
	}
	input := gjson.Get(openAIWSPayloadStringView(payload), "input")
	if !input.Exists() {
		return nil
	}
	var out []string
	appendItem := func(item gjson.Result) {
		if !openAIEncryptedLineageItemType(item.Get("type").String()) {
			return
		}
		encrypted := item.Get("encrypted_content")
		if encrypted.Type == gjson.String && encrypted.String() != "" {
			out = append(out, openAIEncryptedContentDigest(encrypted.String()))
		}
	}
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool { appendItem(item); return true })
		return out
	}
	if input.IsObject() {
		appendItem(input)
	}
	return out
}

func openAIRawPayloadHasInvalidEncryptedContent(payload []byte, invalid map[string]struct{}) bool {
	if len(payload) == 0 || len(invalid) == 0 {
		return false
	}
	input := gjson.Get(openAIWSPayloadStringView(payload), "input")
	check := func(item gjson.Result) bool {
		encrypted := item.Get("encrypted_content")
		if encrypted.Type != gjson.String || encrypted.String() == "" {
			return false
		}
		_, ok := invalid[openAIEncryptedContentDigest(encrypted.String())]
		return ok
	}
	if input.IsArray() {
		hit := false
		input.ForEach(func(_, item gjson.Result) bool { hit = check(item); return !hit })
		return hit
	}
	if input.IsObject() {
		return check(input)
	}
	return false
}

func stripOpenAIInvalidEncryptedContentItems(reqBody map[string]any, invalid map[string]struct{}) int {
	input, ok := reqBody["input"]
	if !ok || len(invalid) == 0 {
		return 0
	}
	strip := func(item any) (any, bool, bool) {
		m, ok := item.(map[string]any)
		if !ok {
			return item, false, true
		}
		encrypted, ok := m["encrypted_content"].(string)
		if !ok || encrypted == "" {
			return item, false, true
		}
		if _, hit := invalid[openAIEncryptedContentDigest(encrypted)]; !hit {
			return item, false, true
		}
		return sanitizeEncryptedReasoningInputItem(item)
	}
	count := 0
	switch values := input.(type) {
	case []any:
		filtered := values[:0]
		for _, item := range values {
			next, changed, keep := strip(item)
			if changed {
				count++
			}
			if keep {
				filtered = append(filtered, next)
			}
		}
		if count > 0 {
			if len(filtered) == 0 {
				delete(reqBody, "input")
			} else {
				reqBody["input"] = filtered
			}
		}
	case map[string]any:
		next, changed, keep := strip(values)
		if changed {
			count++
			if keep {
				reqBody["input"] = next
			} else {
				delete(reqBody, "input")
			}
		}
	}
	return count
}

func stripOpenAIInvalidEncryptedContentRaw(payload []byte, invalid map[string]struct{}) ([]byte, int, error) {
	if !openAIRawPayloadHasInvalidEncryptedContent(payload, invalid) {
		return payload, 0, nil
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload, 0, err
	}
	count := stripOpenAIInvalidEncryptedContentItems(body, invalid)
	if count == 0 {
		return payload, 0, nil
	}
	rebuilt, err := json.Marshal(body)
	if err != nil {
		return payload, 0, err
	}
	return rebuilt, count, nil
}

// stripOpenAIInvalidEncryptedContentFromReplayItems applies the same lineage
// rule to stored replay item bodies. It returns the original slice on a miss,
// preserving the replay store's shared-body ownership contract.
func stripOpenAIInvalidEncryptedContentFromReplayItems(items []json.RawMessage, invalid map[string]struct{}) ([]json.RawMessage, int) {
	if len(items) == 0 || len(invalid) == 0 {
		return items, 0
	}
	hit := false
	for _, item := range items {
		encrypted := gjson.Get(openAIWSPayloadStringView(item), "encrypted_content")
		if encrypted.Type == gjson.String && encrypted.String() != "" {
			if _, ok := invalid[openAIEncryptedContentDigest(encrypted.String())]; ok {
				hit = true
				break
			}
		}
	}
	if !hit {
		return items, 0
	}
	next := make([]json.RawMessage, 0, len(items))
	stripped := 0
	for _, item := range items {
		var decoded map[string]any
		if err := json.Unmarshal(item, &decoded); err != nil {
			next = append(next, item)
			continue
		}
		encrypted, ok := decoded["encrypted_content"].(string)
		if !ok || encrypted == "" {
			next = append(next, item)
			continue
		}
		if _, ok := invalid[openAIEncryptedContentDigest(encrypted)]; !ok {
			next = append(next, item)
			continue
		}
		nextItem, changed, keep := sanitizeEncryptedReasoningInputItem(decoded)
		if !changed {
			next = append(next, item)
			continue
		}
		stripped++
		if !keep {
			continue
		}
		rebuilt, err := json.Marshal(nextItem)
		if err != nil {
			next = append(next, item)
			stripped--
			continue
		}
		next = append(next, json.RawMessage(rebuilt))
	}
	if stripped == 0 {
		return items, 0
	}
	return next, stripped
}

func (s *OpenAIGatewayService) markOpenAIWSInvalidEncryptedContentLineage(groupID int64, sessionHash string, digests []string) {
	if s == nil || strings.TrimSpace(sessionHash) == "" || len(digests) == 0 {
		return
	}
	if store := s.getOpenAIWSStateStore(); store != nil {
		store.MarkSessionInvalidEncryptedContent(groupID, sessionHash, digests, s.openAIWSSessionStickyTTL())
	}
}

func (s *OpenAIGatewayService) sessionInvalidEncryptedContentDigests(groupID int64, sessionHash string) map[string]struct{} {
	if s == nil || strings.TrimSpace(sessionHash) == "" {
		return nil
	}
	store := s.getOpenAIWSStateStore()
	if store == nil || !store.HasAnySessionInvalidEncryptedContent() {
		return nil
	}
	return store.GetSessionInvalidEncryptedContentDigests(groupID, sessionHash)
}

func (s *OpenAIGatewayService) openAIWSLineageSessionHashFromContext(c *gin.Context, body []byte) string {
	if c != nil {
		if value := strings.TrimSpace(c.GetString(openAIWSIngressSessionHashContextKey)); value != "" {
			return value
		}
	}
	return s.GenerateSessionHash(c, body)
}

func (s *OpenAIGatewayService) markOpenAIWSInvalidEncryptedContentLineageFromPayload(c *gin.Context, payload []byte, logKey string, accountID int64, turn int) {
	digests := collectOpenAIEncryptedContentDigestsRaw(payload)
	if len(digests) == 0 {
		return
	}
	s.markOpenAIWSInvalidEncryptedContentLineage(
		getOpenAIGroupIDFromContext(c),
		s.openAIWSLineageSessionHashFromContext(c, payload),
		digests,
	)
	logOpenAIWSModeInfo("%s account_id=%d turn=%d digests=%d", logKey, accountID, turn, len(digests))
}

func (s *OpenAIGatewayService) stripSessionInvalidEncryptedContentLogged(payload []byte, invalid map[string]struct{}, logKey string, accountID int64, turn int) ([]byte, int) {
	strippedPayload, strippedCount, err := stripOpenAIInvalidEncryptedContentRaw(payload, invalid)
	if err != nil {
		logOpenAIWSModeInfo("%s_skip account_id=%d turn=%d reason=strip_error cause=%s", logKey, accountID, turn, truncateOpenAIWSLogValue(err.Error(), openAIWSLogValueMaxLen))
		return payload, 0
	}
	if strippedCount > 0 {
		logOpenAIWSModeInfo("%s account_id=%d turn=%d stripped_items=%d", logKey, accountID, turn, strippedCount)
	}
	return strippedPayload, strippedCount
}
