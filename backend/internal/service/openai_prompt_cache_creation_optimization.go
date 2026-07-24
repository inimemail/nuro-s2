package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const openAIPromptCacheExplicitMinStaticBytes = 4 * 1024
const openAIPromptCacheCreationOptimizationTTL = "30m"

type openAIPromptCacheCreationOptimizationResult struct {
	Applied                     bool
	BreakpointInserted          bool
	RemovedPromptCacheRetention bool
}

// applyOpenAIPromptCacheCreationOptimizationBody is deliberately a byte-exact
// no-op unless the account enabled the policy and the final upstream model
// belongs to GPT-5.6.
func applyOpenAIPromptCacheCreationOptimizationBody(
	account *Account,
	upstreamModel string,
	body []byte,
) ([]byte, openAIPromptCacheCreationOptimizationResult, error) {
	return applyOpenAIPromptCacheCreationOptimizationBodyWithIntent(account, upstreamModel, body, nil)
}

func applyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(
	account *Account,
	upstreamModel string,
	body []byte,
	explicitImageIntent bool,
) ([]byte, openAIPromptCacheCreationOptimizationResult, error) {
	return applyOpenAIPromptCacheCreationOptimizationBodyWithIntent(account, upstreamModel, body, &explicitImageIntent)
}

func applyOpenAIPromptCacheCreationOptimizationBodyWithIntent(
	account *Account,
	upstreamModel string,
	body []byte,
	explicitImageIntent *bool,
) ([]byte, openAIPromptCacheCreationOptimizationResult, error) {
	var result openAIPromptCacheCreationOptimizationResult
	if account == nil || !account.IsOpenAIPromptCacheCreationOptimizationEnabled() || !isOpenAIGPT56Model(upstreamModel) {
		return body, result, nil
	}

	var request map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		return nil, result, fmt.Errorf("parse OpenAI cache creation optimization body: %w", err)
	}
	if err := ensureOpenAIPromptCacheJSONEOF(decoder); err != nil {
		return nil, result, err
	}
	imageIntent := IsExplicitImageGenerationIntentMap(openAIResponsesEndpoint, upstreamModel, request)
	if explicitImageIntent != nil {
		// The ingress marker overrides only tool intent introduced by server-side
		// transforms. A mapped image-only model remains image intent regardless.
		imageIntent = *explicitImageIntent ||
			isOpenAIImageGenerationModel(upstreamModel) ||
			isOpenAIImageGenerationModel(firstNonEmptyString(request["model"]))
	}
	if imageIntent {
		return body, result, nil
	}
	result = applyOpenAIPromptCacheCreationOptimizationMapWithExplicitIntent(account, upstreamModel, request, imageIntent)

	updated, err := json.Marshal(request)
	if err != nil {
		return nil, openAIPromptCacheCreationOptimizationResult{}, fmt.Errorf("serialize OpenAI cache creation optimization body: %w", err)
	}
	return updated, result, nil
}

func ensureOpenAIPromptCacheJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return fmt.Errorf("parse OpenAI cache creation optimization body: %w", err)
	}
	return nil
}

func applyOpenAIPromptCacheCreationOptimizationMap(
	account *Account,
	upstreamModel string,
	request map[string]any,
) openAIPromptCacheCreationOptimizationResult {
	return applyOpenAIPromptCacheCreationOptimizationMapWithExplicitIntent(
		account,
		upstreamModel,
		request,
		IsExplicitImageGenerationIntentMap(openAIResponsesEndpoint, upstreamModel, request),
	)
}

func applyOpenAIPromptCacheCreationOptimizationMapWithExplicitIntent(
	account *Account,
	upstreamModel string,
	request map[string]any,
	explicitImageIntent bool,
) openAIPromptCacheCreationOptimizationResult {
	var result openAIPromptCacheCreationOptimizationResult
	if account == nil || !account.IsOpenAIPromptCacheCreationOptimizationEnabled() || !isOpenAIGPT56Model(upstreamModel) || request == nil {
		return result
	}
	if explicitImageIntent || isOpenAIImageGenerationModel(upstreamModel) || isOpenAIImageGenerationModel(firstNonEmptyString(request["model"])) {
		return result
	}
	result.Applied = true
	result.RemovedPromptCacheRetention = removeOpenAIPromptCacheRetention(request)
	removeOpenAIPromptCacheBreakpoints(request)
	mode := account.OpenAIPromptCacheCreationOptimizationMode()
	promptCacheOptions := map[string]any{
		"mode": "explicit",
		"ttl":  openAIPromptCacheCreationOptimizationTTL,
	}
	if mode == OpenAIPromptCacheCreationOptimizationModeSuppress {
		delete(promptCacheOptions, "ttl")
	}
	request["prompt_cache_options"] = promptCacheOptions
	if mode == OpenAIPromptCacheCreationOptimizationModeReduce {
		if messages, ok := request["messages"].([]any); ok {
			result.BreakpointInserted = insertOpenAIChatStablePrefixBreakpoint(request, messages)
		} else if input, ok := request["input"].([]any); ok {
			result.BreakpointInserted = insertOpenAIResponsesStablePrefixBreakpoint(request, input)
		}
	}
	return result
}

func openAIPromptCacheCreationOptimizationFallbackAccount(account *Account) *Account {
	if account == nil {
		return nil
	}
	fallback := *account
	if account.Credentials == nil {
		return &fallback
	}
	fallback.Credentials = make(map[string]any, len(account.Credentials))
	for key, value := range account.Credentials {
		fallback.Credentials[key] = value
	}
	delete(fallback.Credentials, "openai_prompt_cache_creation_optimization_enabled")
	delete(fallback.Credentials, "openai_prompt_cache_creation_optimization_mode")
	return &fallback
}

// OpenAIPromptCacheCreationOptimizationFallbackAccount returns an account copy
// with this optional request policy disabled. It is used by edge-rs control
// plane retries without mutating the scheduler's shared account object.
func OpenAIPromptCacheCreationOptimizationFallbackAccount(account *Account) *Account {
	return openAIPromptCacheCreationOptimizationFallbackAccount(account)
}

func isOpenAIPromptCacheCreationOptimizationUnsupportedError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode < 400 || statusCode >= 500 {
		return false
	}
	for _, candidate := range openAIPromptCacheCreationOptimizationErrorCandidates(upstreamMsg, upstreamBody) {
		if openAIPromptCacheCreationOptimizationErrorCandidateUnsupported(candidate) {
			return true
		}
	}
	return false
}

func openAIPromptCacheCreationOptimizationErrorCandidates(upstreamMsg string, upstreamBody []byte) []string {
	candidates := make([]string, 0, 5)
	if message := strings.TrimSpace(upstreamMsg); message != "" {
		messageBytes := []byte(message)
		if json.Valid(messageBytes) {
			// edge-rs reports the complete JSON error body in error_message.
			// Parse it structurally instead of scanning echoed request fields.
			if len(upstreamBody) == 0 {
				upstreamBody = messageBytes
			}
		} else {
			candidates = append(candidates, message)
		}
	}
	if len(upstreamBody) == 0 {
		return candidates
	}

	var payload any
	if !json.Valid(upstreamBody) || json.Unmarshal(upstreamBody, &payload) != nil {
		// Plain-text proxy errors can still identify an unsupported field, but
		// avoid scanning an arbitrarily large response or an echoed request body.
		if len(upstreamBody) <= 4*1024 {
			if body := strings.TrimSpace(string(upstreamBody)); body != "" {
				candidates = append(candidates, body)
			}
		}
		return candidates
	}

	root, ok := payload.(map[string]any)
	if !ok {
		if message, ok := payload.(string); ok && strings.TrimSpace(message) != "" {
			candidates = append(candidates, message)
		}
		return candidates
	}
	candidates = appendOpenAIPromptCacheCreationOptimizationErrorObject(candidates, root)
	switch detail := root["detail"].(type) {
	case string:
		if strings.TrimSpace(detail) != "" {
			candidates = append(candidates, detail)
		}
	case []any:
		candidates = appendOpenAIPromptCacheCreationOptimizationErrorList(candidates, detail)
	}
	if errors, ok := root["errors"].([]any); ok {
		candidates = appendOpenAIPromptCacheCreationOptimizationErrorList(candidates, errors)
	}
	if errorText, ok := root["error"].(string); ok && strings.TrimSpace(errorText) != "" {
		candidates = append(candidates, errorText)
	}
	if errorObject, ok := root["error"].(map[string]any); ok {
		candidates = appendOpenAIPromptCacheCreationOptimizationErrorObject(candidates, errorObject)
	}
	if response, ok := root["response"].(map[string]any); ok {
		if errorText, ok := response["error"].(string); ok && strings.TrimSpace(errorText) != "" {
			candidates = append(candidates, errorText)
		}
		if errorObject, ok := response["error"].(map[string]any); ok {
			candidates = appendOpenAIPromptCacheCreationOptimizationErrorObject(candidates, errorObject)
		}
	}
	return candidates
}

func appendOpenAIPromptCacheCreationOptimizationErrorObject(candidates []string, object map[string]any) []string {
	parts := make([]string, 0, 8)
	for _, field := range []string{"message", "msg", "detail", "type", "code", "param"} {
		if value, ok := object[field].(string); ok && strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	if location, ok := object["loc"].([]any); ok {
		for _, rawPart := range location {
			if value, ok := rawPart.(string); ok && strings.TrimSpace(value) != "" {
				parts = append(parts, value)
			}
		}
	}
	if len(parts) > 0 {
		candidates = append(candidates, strings.Join(parts, " "))
	}
	return candidates
}

func appendOpenAIPromptCacheCreationOptimizationErrorList(candidates []string, list []any) []string {
	for _, rawItem := range list {
		if item, ok := rawItem.(map[string]any); ok {
			candidates = appendOpenAIPromptCacheCreationOptimizationErrorObject(candidates, item)
		}
	}
	return candidates
}

func openAIPromptCacheCreationOptimizationErrorCandidateUnsupported(candidate string) bool {
	const contextBytes = 512
	text := strings.NewReplacer("_", " ", "-", " ").Replace(strings.ToLower(candidate))
	fields := []string{
		"prompt cache options",
		"prompt cache breakpoint",
	}
	markers := []string{
		"unsupported parameter",
		"unknown parameter",
		"unrecognized parameter",
		"invalid parameter",
		"unknown field",
		"unrecognized field",
		"unexpected field",
		"extra inputs are not permitted",
		"additional properties are not allowed",
		"not permitted",
		"not allowed",
		"not supported",
		"unsupported",
		"invalid value",
		"invalid field",
		"must be",
	}
	for _, field := range fields {
		for searchFrom := 0; searchFrom < len(text); {
			relative := strings.Index(text[searchFrom:], field)
			if relative < 0 {
				break
			}
			fieldStart := searchFrom + relative
			windowStart := max(0, fieldStart-contextBytes)
			windowEnd := min(len(text), fieldStart+len(field)+contextBytes)
			if containsAnySubstring(text[windowStart:windowEnd], markers...) {
				return true
			}
			searchFrom = fieldStart + len(field)
		}
	}
	return false
}

// IsOpenAIPromptCacheCreationOptimizationUnsupportedError identifies a 4xx
// caused specifically by the optional cache policy fields.
func IsOpenAIPromptCacheCreationOptimizationUnsupportedError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	return isOpenAIPromptCacheCreationOptimizationUnsupportedError(statusCode, upstreamMsg, upstreamBody)
}

func removeOpenAIPromptCacheRetention(request map[string]any) bool {
	if _, exists := request["prompt_cache_retention"]; !exists {
		return false
	}
	delete(request, "prompt_cache_retention")
	return true
}

func removeOpenAIPromptCacheBreakpoints(value any) bool {
	request, ok := value.(map[string]any)
	if !ok || request == nil {
		return false
	}
	changed := removeOpenAIPromptCacheBreakpointField(request)
	for _, field := range []string{"input", "messages", "instructions"} {
		changed = removeOpenAIPromptCacheBreakpointsFromSequence(request[field]) || changed
	}
	return changed
}

func removeOpenAIPromptCacheBreakpointsFromSequence(value any) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		changed = removeOpenAIPromptCacheBreakpointField(item) || changed
		switch content := item["content"].(type) {
		case map[string]any:
			changed = removeOpenAIPromptCacheBreakpointField(content) || changed
		case []any:
			for _, rawPart := range content {
				if part, ok := rawPart.(map[string]any); ok {
					changed = removeOpenAIPromptCacheBreakpointField(part) || changed
				}
			}
		}
	}
	return changed
}

func removeOpenAIPromptCacheBreakpointField(value map[string]any) bool {
	if _, exists := value["prompt_cache_breakpoint"]; !exists {
		return false
	}
	delete(value, "prompt_cache_breakpoint")
	return true
}

func insertOpenAIResponsesStablePrefixBreakpoint(request map[string]any, input []any) bool {
	stableBytes := openAIPromptCacheTopLevelStaticBytes(request)
	var target *openAIPromptCacheStableTarget
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			break
		}
		role := strings.ToLower(strings.TrimSpace(stringValue(item["role"])))
		if role != "system" && role != "developer" {
			break
		}
		candidate, size, safe := openAIStableContentTarget(item, "input_text", map[string]bool{
			"input_text":  true,
			"input_image": true,
			"input_file":  true,
		})
		stableBytes += size
		if candidate != nil {
			target = candidate
		}
		if !safe {
			break
		}
	}
	if stableBytes < openAIPromptCacheExplicitMinStaticBytes || target == nil {
		return false
	}
	return target.insertBreakpoint()
}

func insertOpenAIChatStablePrefixBreakpoint(request map[string]any, messages []any) bool {
	stableBytes := openAIPromptCacheTopLevelStaticBytes(request)
	var target *openAIPromptCacheStableTarget
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			break
		}
		role := strings.ToLower(strings.TrimSpace(stringValue(message["role"])))
		if role != "system" && role != "developer" {
			break
		}
		candidate, size, safe := openAIStableContentTarget(message, "text", map[string]bool{
			"text":        true,
			"image_url":   true,
			"input_audio": true,
			"file":        true,
			"refusal":     true,
		})
		stableBytes += size
		if candidate != nil {
			target = candidate
		}
		if !safe {
			break
		}
	}
	if stableBytes < openAIPromptCacheExplicitMinStaticBytes || target == nil {
		return false
	}
	return target.insertBreakpoint()
}

type openAIPromptCacheStableTarget struct {
	part              map[string]any
	container         map[string]any
	stringContentType string
	stringContent     string
}

func (t *openAIPromptCacheStableTarget) insertBreakpoint() bool {
	if t == nil {
		return false
	}
	part := t.part
	if part == nil {
		if t.container == nil || t.stringContentType == "" || strings.TrimSpace(t.stringContent) == "" {
			return false
		}
		part = map[string]any{
			"type": t.stringContentType,
			"text": t.stringContent,
		}
		t.container["content"] = []any{part}
	}
	part["prompt_cache_breakpoint"] = map[string]any{"mode": "explicit"}
	return true
}

func openAIStableContentTarget(
	container map[string]any,
	stringContentType string,
	supportedTypes map[string]bool,
) (*openAIPromptCacheStableTarget, int, bool) {
	content, exists := container["content"]
	if !exists {
		return nil, 0, false
	}
	switch typed := content.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, 0, true
		}
		return &openAIPromptCacheStableTarget{
			container:         container,
			stringContentType: stringContentType,
			stringContent:     typed,
		}, len(typed), true
	case []any:
		var target *openAIPromptCacheStableTarget
		size := 0
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return target, size, false
			}
			partType := strings.ToLower(strings.TrimSpace(stringValue(part["type"])))
			if !supportedTypes[partType] {
				return target, size, false
			}
			size += jsonValueSize(part)
			target = &openAIPromptCacheStableTarget{part: part}
		}
		return target, size, true
	default:
		return nil, 0, false
	}
}

func openAIPromptCacheTopLevelStaticBytes(request map[string]any) int {
	total := 0
	if instructions, ok := request["instructions"].(string); ok {
		total += len(instructions)
	}
	for _, field := range []string{"tools", "functions", "response_format", "text"} {
		if value, exists := request[field]; exists {
			total += jsonValueSize(value)
		}
	}
	return total
}

func jsonValueSize(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
