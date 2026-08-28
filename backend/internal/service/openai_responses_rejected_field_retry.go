package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	maxOpenAIResponsesRejectedFieldRetries         = 6
	openAIResponsesRejectedFieldCacheTTL           = 10 * time.Minute
	openAIResponsesRejectedFieldRemotePollInterval = time.Second
	openAIResponsesRejectedFieldCacheRedisPrefix   = "openai:responses_rejected_fields:v1:"
	openAIResponsesRejectedFieldMaxOutputTokens    = "max_output_tokens"
	openAIResponsesRejectedFieldInputNamespace     = "input.namespace"
	openAIResponsesRejectedFieldInputStatus        = "input.status"
)

var (
	openAIResponsesRejectedNamespaceParamPattern = regexp.MustCompile(`(?i)^input\[(\d+)\]\.namespace$`)
	openAIResponsesRejectedStatusParamPattern    = regexp.MustCompile(`(?i)^input\[(\d+)\]\.status$`)
	openAIResponsesRejectedMessageParamPattern   = regexp.MustCompile(`(?i)(?:unknown|unsupported)[ _-]+parameter\s*(?::|=|is)?\s*["']?(max_output_tokens|input\[\d+\]\.(?:namespace|status))(?:["']|\b)`)
)

type openAIResponsesRejectedFieldRetryState struct {
	attempts       int
	seenBodyHashes map[[sha256.Size]byte]struct{}
}

// OpenAIResponsesRejectedFieldRetryState bounds compatibility retries for an
// upstream that explicitly rejects a supported Responses request field. The
// zero value is ready to use and allocates only after the first valid rewrite.
type OpenAIResponsesRejectedFieldRetryState struct {
	state openAIResponsesRejectedFieldRetryState
}

func canonicalOpenAIResponsesRejectedField(field string) string {
	field = strings.ToLower(strings.TrimSpace(field))
	switch {
	case field == openAIResponsesRejectedFieldMaxOutputTokens,
		field == openAIResponsesRejectedFieldInputNamespace,
		field == openAIResponsesRejectedFieldInputStatus:
		return field
	case openAIResponsesRejectedNamespaceParamPattern.MatchString(field):
		return openAIResponsesRejectedFieldInputNamespace
	case openAIResponsesRejectedStatusParamPattern.MatchString(field):
		return openAIResponsesRejectedFieldInputStatus
	default:
		return ""
	}
}

func openAIResponsesRejectedFieldCapabilityKey(account *Account, model, transport string) string {
	if account == nil || account.ID <= 0 || account.Platform != PlatformOpenAI {
		return ""
	}
	material := strings.Join([]string{
		strconv.FormatInt(account.ID, 10),
		strings.ToLower(strings.TrimSpace(account.Type)),
		strings.ToLower(strings.TrimRight(strings.TrimSpace(account.GetOpenAIBaseURL()), "/")),
		strings.ToLower(strings.TrimSpace(transport)),
		strings.ToLower(strings.TrimSpace(model)),
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

func openAIResponsesRejectedFieldMemoryKey(capabilityKey, field string) string {
	return capabilityKey + ":" + field
}

// OpenAIResponsesRejectedFieldTransportScope returns the capability-cache
// namespace for a Responses transport. Compact and regular Responses requests
// intentionally use separate scopes because upstreams may support different
// fields on the two endpoints.
func OpenAIResponsesRejectedFieldTransportScope(transport string, compact bool) string {
	scope := strings.ToLower(strings.TrimSpace(transport))
	if compact {
		return scope + ":compact"
	}
	return scope + ":responses"
}

func openAIResponsesRejectedFieldTransportScope(transport string, compact bool) string {
	return OpenAIResponsesRejectedFieldTransportScope(transport, compact)
}

// RecordOpenAIResponsesRejectedField remembers only an explicit, parsed 400
// compatibility rejection. The cache is scoped tightly enough that changing
// account endpoint, transport, or model naturally starts with a clean state.
func (s *OpenAIGatewayService) RecordOpenAIResponsesRejectedField(account *Account, model, transport, field string) {
	if s == nil {
		return
	}
	capabilityKey := openAIResponsesRejectedFieldCapabilityKey(account, model, transport)
	field = canonicalOpenAIResponsesRejectedField(field)
	if capabilityKey == "" || field == "" {
		return
	}
	until := time.Now().Add(openAIResponsesRejectedFieldCacheTTL)
	memoryKey := openAIResponsesRejectedFieldMemoryKey(capabilityKey, field)
	s.storeOpenAIResponsesRejectedFieldUntil(memoryKey, until)
	if s.openaiAccountHealthRedis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	pipe := s.openaiAccountHealthRedis.TxPipeline()
	key := openAIResponsesRejectedFieldCacheRedisPrefix + capabilityKey
	pipe.HSet(ctx, key, field, until.Unix())
	pipe.Expire(ctx, key, openAIResponsesRejectedFieldCacheTTL)
	_, _ = pipe.Exec(ctx)
	cancel()
}

func (s *OpenAIGatewayService) openAIResponsesRejectedFields(account *Account, model, transport string) []string {
	if s == nil {
		return nil
	}
	capabilityKey := openAIResponsesRejectedFieldCapabilityKey(account, model, transport)
	if capabilityKey == "" {
		return nil
	}
	now := time.Now()
	if s.openaiAccountHealthRedis != nil && s.shouldPollOpenAIResponsesRejectedFields(capabilityKey, now) {
		// Remote capability sharing is a best-effort optimization. Refresh it
		// asynchronously so Redis latency cannot extend a normal request's TTFT.
		go s.refreshOpenAIResponsesRejectedFields(capabilityKey, now)
	}
	fields := make([]string, 0, 3)
	for _, field := range []string{
		openAIResponsesRejectedFieldMaxOutputTokens,
		openAIResponsesRejectedFieldInputNamespace,
		openAIResponsesRejectedFieldInputStatus,
	} {
		memoryKey := openAIResponsesRejectedFieldMemoryKey(capabilityKey, field)
		rawUntil, ok := s.openaiResponsesRejectedFieldUntil.Load(memoryKey)
		until, valid := rawUntil.(time.Time)
		if !ok || !valid || !now.Before(until) {
			if ok {
				s.openaiResponsesRejectedFieldUntil.Delete(memoryKey)
			}
			continue
		}
		fields = append(fields, field)
	}
	return fields
}

func (s *OpenAIGatewayService) refreshOpenAIResponsesRejectedFields(capabilityKey string, checkedAt time.Time) {
	if s == nil || s.openaiAccountHealthRedis == nil || capabilityKey == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	values, err := s.openaiAccountHealthRedis.HGetAll(ctx, openAIResponsesRejectedFieldCacheRedisPrefix+capabilityKey).Result()
	cancel()
	if err == nil {
		now := time.Now()
		for field, rawUntil := range values {
			field = canonicalOpenAIResponsesRejectedField(field)
			untilUnix, parseErr := strconv.ParseInt(strings.TrimSpace(rawUntil), 10, 64)
			if field == "" || parseErr != nil || untilUnix <= now.Unix() {
				continue
			}
			until := time.Unix(untilUnix, 0)
			memoryKey := openAIResponsesRejectedFieldMemoryKey(capabilityKey, field)
			s.storeOpenAIResponsesRejectedFieldUntil(memoryKey, until)
		}
	}
	// Bound cleanup timer retention for hot capability scopes. Scheduling a
	// ten-minute timer on every one-second refresh would leave hundreds of
	// no-op timers alive for each active account/model/transport tuple.
	time.AfterFunc(2*openAIResponsesRejectedFieldRemotePollInterval, func() {
		s.openaiResponsesRejectedFieldRemoteCheckedAt.CompareAndDelete(capabilityKey, checkedAt)
	})
}

func (s *OpenAIGatewayService) shouldPollOpenAIResponsesRejectedFields(capabilityKey string, now time.Time) bool {
	for {
		raw, loaded := s.openaiResponsesRejectedFieldRemoteCheckedAt.LoadOrStore(capabilityKey, now)
		if !loaded {
			return true
		}
		checkedAt, valid := raw.(time.Time)
		if valid && now.Sub(checkedAt) < openAIResponsesRejectedFieldRemotePollInterval {
			return false
		}
		if !valid {
			s.openaiResponsesRejectedFieldRemoteCheckedAt.Delete(capabilityKey)
			continue
		}
		if s.openaiResponsesRejectedFieldRemoteCheckedAt.CompareAndSwap(capabilityKey, checkedAt, now) {
			return true
		}
	}
}

func (s *OpenAIGatewayService) storeOpenAIResponsesRejectedFieldUntil(memoryKey string, until time.Time) {
	if s == nil || memoryKey == "" || until.IsZero() {
		return
	}
	for {
		raw, loaded := s.openaiResponsesRejectedFieldUntil.LoadOrStore(memoryKey, until)
		if !loaded {
			break
		}
		current, valid := raw.(time.Time)
		if valid && !until.After(current) {
			return
		}
		if !valid {
			s.openaiResponsesRejectedFieldUntil.Delete(memoryKey)
			continue
		}
		if s.openaiResponsesRejectedFieldUntil.CompareAndSwap(memoryKey, current, until) {
			break
		}
	}
	// Keep one cleanup worker per capability field. The worker follows deadline
	// extensions, so repeated rejections do not accumulate timers and expired
	// low-traffic entries do not remain in memory indefinitely.
	if _, loaded := s.openaiResponsesRejectedFieldCleanupScheduled.LoadOrStore(memoryKey, struct{}{}); !loaded {
		go s.cleanupOpenAIResponsesRejectedFieldUntil(memoryKey)
	}
}

func (s *OpenAIGatewayService) cleanupOpenAIResponsesRejectedFieldUntil(memoryKey string) {
	for {
		rawUntil, ok := s.openaiResponsesRejectedFieldUntil.Load(memoryKey)
		if !ok {
			s.releaseOpenAIResponsesRejectedFieldCleanup(memoryKey)
			return
		}
		until, valid := rawUntil.(time.Time)
		if !valid {
			if s.openaiResponsesRejectedFieldUntil.CompareAndDelete(memoryKey, rawUntil) {
				continue
			}
			continue
		}
		delay := time.Until(until)
		if delay > 0 {
			timer := time.NewTimer(delay)
			<-timer.C
		}

		// A newer rejection may have extended the deadline while this worker
		// was waiting. CompareAndDelete prevents an old worker wake-up from
		// deleting that newer deadline.
		if s.openaiResponsesRejectedFieldUntil.CompareAndDelete(memoryKey, until) {
			s.releaseOpenAIResponsesRejectedFieldCleanup(memoryKey)
			return
		}
	}
}

func (s *OpenAIGatewayService) releaseOpenAIResponsesRejectedFieldCleanup(memoryKey string) {
	// Remove the marker before checking for a replacement value. A concurrent
	// store then either sees the marker and is covered by this worker, or sees
	// no marker and starts its own worker; this ordering avoids a gap with a
	// value but no cleanup worker.
	s.openaiResponsesRejectedFieldCleanupScheduled.LoadAndDelete(memoryKey)
	if _, exists := s.openaiResponsesRejectedFieldUntil.Load(memoryKey); exists {
		if _, loaded := s.openaiResponsesRejectedFieldCleanupScheduled.LoadOrStore(memoryKey, struct{}{}); !loaded {
			go s.cleanupOpenAIResponsesRejectedFieldUntil(memoryKey)
		}
	}
}

// ApplyOpenAIResponsesRejectedFieldCache strips compatibility metadata that
// this exact upstream capability scope explicitly rejected recently.
func (s *OpenAIGatewayService) ApplyOpenAIResponsesRejectedFieldCache(account *Account, model, transport string, body []byte) ([]byte, bool, error) {
	updated := body
	changed := false
	for _, field := range s.openAIResponsesRejectedFields(account, model, transport) {
		next, removed, err := removeOpenAIResponsesRejectedFieldCategory(updated, field)
		if err != nil {
			return nil, false, err
		}
		if removed {
			updated = next
			changed = true
		}
	}
	return updated, changed, nil
}

func removeOpenAIResponsesRejectedFieldCategory(body []byte, field string) ([]byte, bool, error) {
	field = canonicalOpenAIResponsesRejectedField(field)
	if field == openAIResponsesRejectedFieldMaxOutputTokens {
		return RemoveOpenAIResponsesRejectedField(body, field)
	}
	if field != openAIResponsesRejectedFieldInputNamespace && field != openAIResponsesRejectedFieldInputStatus {
		return body, false, nil
	}
	updated := body
	changed := false
	for index, item := range gjson.GetBytes(body, "input").Array() {
		if !item.IsObject() {
			continue
		}
		property := "status"
		if field == openAIResponsesRejectedFieldInputNamespace {
			switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
			case "function_call", "tool_call", "custom_tool_call", "mcp_tool_call":
			default:
				continue
			}
			property = "namespace"
		}
		path := fmt.Sprintf("input.%d.%s", index, property)
		if !gjson.GetBytes(updated, path).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(updated, path)
		if err != nil {
			return nil, false, fmt.Errorf("delete cached rejected field %s: %w", path, err)
		}
		updated = next
		changed = true
	}
	return updated, changed, nil
}

// Rewrite removes exactly one explicitly rejected field and rejects duplicate
// or excessive body variants. Callers must only retry before writing downstream.
func (s *OpenAIResponsesRejectedFieldRetryState) Rewrite(statusCode int, currentBody, responseBody []byte) ([]byte, string, bool, error) {
	if s == nil {
		return nil, "", false, nil
	}
	nextBody, field, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(statusCode, currentBody, responseBody)
	if err != nil || !changed {
		return nextBody, field, false, err
	}
	if !s.state.Allow(currentBody, nextBody) {
		return nil, "", false, nil
	}
	return nextBody, field, true, nil
}

func (s *openAIResponsesRejectedFieldRetryState) Allow(currentBody, nextBody []byte) bool {
	if s == nil || len(nextBody) == 0 || s.attempts >= maxOpenAIResponsesRejectedFieldRetries {
		return false
	}
	if s.seenBodyHashes == nil {
		s.seenBodyHashes = make(map[[sha256.Size]byte]struct{}, maxOpenAIResponsesRejectedFieldRetries+1)
		s.seenBodyHashes[sha256.Sum256(currentBody)] = struct{}{}
	}
	bodyHash := sha256.Sum256(nextBody)
	if _, seen := s.seenBodyHashes[bodyHash]; seen {
		return false
	}
	s.seenBodyHashes[bodyHash] = struct{}{}
	s.attempts++
	return true
}

func normalizeOpenAIResponsesRejectedFieldRetryBody(statusCode int, body, responseBody []byte) ([]byte, string, bool, error) {
	if statusCode != http.StatusBadRequest || len(body) == 0 || len(responseBody) == 0 {
		return nil, "", false, nil
	}
	code := strings.ToLower(strings.TrimSpace(openAIResponsesRejectedErrorField(responseBody, "code")))
	message := strings.ToLower(strings.TrimSpace(openAIResponsesRejectedErrorField(responseBody, "message")))
	if !isExplicitOpenAIResponsesFieldRejection(code, message) {
		return nil, "", false, nil
	}
	param := strings.ToLower(strings.TrimSpace(openAIResponsesRejectedErrorField(responseBody, "param")))
	if param == "" {
		param = openAIResponsesRejectedParamFromMessage(message)
	}
	if index, ok := openAIResponsesRejectedNamespaceIndex(param); ok {
		return removeOpenAIResponsesRejectedNamespaceAtIndex(body, index)
	}
	if index, ok := openAIResponsesRejectedStatusIndex(param); ok {
		return removeOpenAIResponsesRejectedStatusAtIndex(body, index)
	}
	if param == "max_output_tokens" && gjson.GetBytes(body, "max_output_tokens").Exists() {
		retryBody, changed, err := RemoveOpenAIResponsesRejectedField(body, param)
		return retryBody, param, changed, err
	}
	return nil, "", false, nil
}

// RemoveOpenAIResponsesRejectedField removes a previously confirmed
// compatibility field from a Responses body without consuming retry budget.
// max_tokens is the local compatibility source for max_output_tokens, so both
// aliases must be removed before a plan is rebuilt for another account.
func RemoveOpenAIResponsesRejectedField(body []byte, field string) ([]byte, bool, error) {
	field = strings.ToLower(strings.TrimSpace(field))
	if field == "max_output_tokens" {
		updated := body
		changed := false
		for _, alias := range []string{"max_output_tokens", "max_tokens"} {
			if !gjson.GetBytes(updated, alias).Exists() {
				continue
			}
			next, err := sjson.DeleteBytes(updated, alias)
			if err != nil {
				return nil, false, fmt.Errorf("delete rejected %s: %w", alias, err)
			}
			updated = next
			changed = true
		}
		return updated, changed, nil
	}
	if index, ok := openAIResponsesRejectedNamespaceIndex(field); ok {
		updated, _, changed, err := removeOpenAIResponsesRejectedNamespaceAtIndex(body, index)
		return updated, changed, err
	}
	if index, ok := openAIResponsesRejectedStatusIndex(field); ok {
		updated, _, changed, err := removeOpenAIResponsesRejectedStatusAtIndex(body, index)
		return updated, changed, err
	}
	return body, false, nil
}

func openAIResponsesRejectedErrorField(responseBody []byte, field string) string {
	if len(responseBody) == 0 || strings.TrimSpace(field) == "" {
		return ""
	}
	if value := strings.TrimSpace(gjson.GetBytes(responseBody, "error."+field).String()); value != "" {
		return value
	}
	return strings.TrimSpace(gjson.GetBytes(responseBody, "response.error."+field).String())
}

func isExplicitOpenAIResponsesFieldRejection(code, message string) bool {
	switch strings.TrimSpace(code) {
	case "unknown_parameter", "unsupported_parameter":
		return true
	}
	return strings.Contains(message, "unknown parameter") || strings.Contains(message, "unsupported parameter")
}

func openAIResponsesRejectedParamFromMessage(message string) string {
	match := openAIResponsesRejectedMessageParamPattern.FindStringSubmatch(strings.TrimSpace(message))
	if len(match) != 2 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(match[1]))
}

func openAIResponsesRejectedNamespaceIndex(param string) (int, bool) {
	match := openAIResponsesRejectedNamespaceParamPattern.FindStringSubmatch(strings.TrimSpace(param))
	if len(match) != 2 {
		return 0, false
	}
	index, err := strconv.Atoi(match[1])
	return index, err == nil && index >= 0
}

func openAIResponsesRejectedStatusIndex(param string) (int, bool) {
	match := openAIResponsesRejectedStatusParamPattern.FindStringSubmatch(strings.TrimSpace(param))
	if len(match) != 2 {
		return 0, false
	}
	index, err := strconv.Atoi(match[1])
	return index, err == nil && index >= 0
}

// Remove status from all input items sharing the rejected item's type. Long
// conversations commonly contain many same-type items, and one-at-a-time
// cleanup would exhaust the bounded compatibility retry budget.
func removeOpenAIResponsesRejectedStatusAtIndex(body []byte, index int) ([]byte, string, bool, error) {
	itemPath := fmt.Sprintf("input.%d", index)
	rejected := gjson.GetBytes(body, itemPath)
	if !rejected.IsObject() || !gjson.GetBytes(body, itemPath+".status").Exists() {
		return nil, "", false, nil
	}
	rejectedType := strings.TrimSpace(rejected.Get("type").String())
	retryBody := body
	cleared := 0
	if rejectedType != "" {
		for itemIndex, item := range gjson.GetBytes(body, "input").Array() {
			if !item.IsObject() || strings.TrimSpace(item.Get("type").String()) != rejectedType {
				continue
			}
			statusPath := fmt.Sprintf("input.%d.status", itemIndex)
			if !gjson.GetBytes(retryBody, statusPath).Exists() {
				continue
			}
			next, err := sjson.DeleteBytes(retryBody, statusPath)
			if err != nil {
				return nil, "", false, fmt.Errorf("delete rejected status at input[%d]: %w", itemIndex, err)
			}
			retryBody = next
			cleared++
		}
	}
	if cleared == 0 {
		next, err := sjson.DeleteBytes(retryBody, itemPath+".status")
		if err != nil {
			return nil, "", false, fmt.Errorf("delete rejected status at input[%d]: %w", index, err)
		}
		retryBody = next
	}
	return retryBody, fmt.Sprintf("input[%d].status", index), true, nil
}

func removeOpenAIResponsesRejectedNamespaceAtIndex(body []byte, index int) ([]byte, string, bool, error) {
	itemPath := fmt.Sprintf("input.%d", index)
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, itemPath+".type").String())) {
	case "function_call", "tool_call", "custom_tool_call", "mcp_tool_call":
	default:
		return nil, "", false, nil
	}
	namespacePath := itemPath + ".namespace"
	if !gjson.GetBytes(body, namespacePath).Exists() {
		return nil, "", false, nil
	}
	retryBody, err := sjson.DeleteBytes(body, namespacePath)
	if err != nil {
		return nil, "", false, fmt.Errorf("delete rejected namespace at input[%d]: %w", index, err)
	}
	return retryBody, fmt.Sprintf("input[%d].namespace", index), true, nil
}
