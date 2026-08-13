package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/tidwall/gjson"
)

const openaiResponsesProbeTimeout = 15 * time.Second
const responsesProbeMaxBodyBytes = 256 * 1024
const openaiResponsesProbeMaxOutputTokens = 512

// openaiResponsesProbePayload 是探测使用的最小 Responses 请求体。
// 仅作能力探测，不期望响应内容质量；Stream=false 减少 SSE 解析开销。
//
// 探测不仅区分端点是否存在，还验证 Responses function call 能力。部分兼容
// 上游会接受 /responses，却无法执行 Codex 所需的工具调用。
func openaiResponsesProbePayload(modelID string) []byte {
	if strings.TrimSpace(modelID) == "" {
		modelID = openai.DefaultTestModel
	}
	body, _ := json.Marshal(map[string]any{
		"model": modelID,
		"input": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "Call the probe_ping function with ok=true to acknowledge readiness. You must use the tool."},
				},
			},
		},
		"tools": []map[string]any{{
			"type": "function", "name": "probe_ping", "description": "Capability probe. Call to acknowledge.",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}},
		}},
		"tool_choice": "required", "max_output_tokens": openaiResponsesProbeMaxOutputTokens, "stream": false,
	})
	return body
}

func selectResponsesProbeModel(account *Account) string {
	mapping := account.GetModelMapping()
	candidates := make([]string, 0, len(mapping))
	for _, upstream := range mapping {
		upstream = strings.TrimSpace(upstream)
		if upstream != "" && !strings.Contains(upstream, "*") {
			candidates = append(candidates, upstream)
		}
	}
	if len(candidates) == 0 {
		return openai.DefaultTestModel
	}
	sort.Strings(candidates)
	return candidates[0]
}

// ProbeOpenAIAPIKeyResponsesSupport 探测 OpenAI APIKey 账号上游是否支持
// /v1/responses 端点，并将结果持久化到 accounts.extra.openai_responses_supported。
//
// 调用时机：账号创建/更新后，且仅当 platform=openai && type=apikey 时。
//
// 探测策略（参见包文档 internal/pkg/openai_compat）：
//   - 上游 404 / 405 → 不支持，写 false
//   - 上游 2xx 且返回 function_call → 支持，写 true
//   - 上游 2xx 但未调用工具 → 不支持，写 false
//   - 其他 HTTP 状态表示端点存在 → 支持，写 true
//   - failed 或因 max_output_tokens incomplete → 不写标记，保持 unknown
//   - 网络层失败（连接错误、超时）→ 不写标记，保持 unknown
//     （后续请求仍按"现状即证据"默认走 Responses）
//
// 该方法是幂等的：重复调用会以最新探测结果覆盖标记。
//
// 关于失败处理：探测本身的失败不应阻塞账号创建——账号能创建/更新成功就够了，
// 探测结果只影响后续路由优化。所有错误都仅记录日志，不向调用方传播。
func (s *AccountTestService) ProbeOpenAIAPIKeyResponsesSupport(ctx context.Context, accountID int64) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("service.openai_probe", "probe_load_account_failed: account_id=%d err=%v", accountID, err)
		return
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey {
		// 仅 OpenAI APIKey 账号需要探测；其他账号类型无能力差异。
		return
	}

	apiKey := account.GetOpenAIApiKey()
	if apiKey == "" {
		logger.LegacyPrintf("service.openai_probe", "probe_skip_no_apikey: account_id=%d", accountID)
		return
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		logger.LegacyPrintf("service.openai_probe", "probe_invalid_baseurl: account_id=%d base_url=%q err=%v", accountID, baseURL, err)
		return
	}

	probeURL := buildOpenAIResponsesURL(normalizedBaseURL)
	probeModel := selectResponsesProbeModel(account)

	probeCtx, cancel := context.WithTimeout(ctx, openaiResponsesProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, probeURL, bytes.NewReader(openaiResponsesProbePayload(probeModel)))
	if err != nil {
		logger.LegacyPrintf("service.openai_probe", "probe_build_request_failed: account_id=%d err=%v", accountID, err)
		return
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	applyOpenAICodexProbeHeaders(req.Header)
	// 账号级请求头覆写：能力探测与真实转发保持一致的最终头
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		// 网络层失败：不写标记，保持 unknown，下次重试或由网关 fallback 处理
		logger.LegacyPrintf("service.openai_probe", "probe_request_failed: account_id=%d url=%s err=%v", accountID, probeURL, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, responsesProbeMaxBodyBytes))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responsesProbeMaxBodyBytes))
	if readErr != nil {
		logger.LegacyPrintf("service.openai_probe", "probe_read_body_failed: account_id=%d url=%s err=%v", accountID, probeURL, readErr)
		return
	}
	if !responsesProbeVerdictIsConclusive(resp.StatusCode, bodyBytes) {
		logger.LegacyPrintf("service.openai_probe", "probe_inconclusive_keep_unknown: account_id=%d base_url=%s probe_model=%s status=%d response_status=%s reason=%s", accountID, normalizedBaseURL, probeModel, resp.StatusCode, gjson.GetBytes(bodyBytes, "status").String(), gjson.GetBytes(bodyBytes, "incomplete_details.reason").String())
		return
	}
	supported := decideResponsesProbeSupport(resp.StatusCode, bodyBytes)

	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
		openai_compat.ExtraKeyResponsesSupported: supported,
	}); err != nil {
		logger.LegacyPrintf("service.openai_probe", "probe_persist_failed: account_id=%d supported=%v err=%v", accountID, supported, err)
		return
	}

	if !supported {
		slog.Warn("openai_responses_probe_marked_unsupported", "account_id", accountID, "account_name", account.Name, "base_url", normalizedBaseURL, "probe_model", probeModel, "upstream_status", resp.StatusCode)
	}
	logger.LegacyPrintf("service.openai_probe", "probe_done: account_id=%d base_url=%s probe_model=%s status=%d supported=%v", accountID, normalizedBaseURL, probeModel, resp.StatusCode, supported)
}

// isResponsesEndpointSupportedByStatus only identifies endpoint presence.
// Successful responses still need decideResponsesProbeSupport to verify tools.
func isResponsesEndpointSupportedByStatus(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return false
	}
	return true
}

func decideResponsesProbeSupport(status int, body []byte) bool {
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return false
	}
	if status < 200 || status >= 300 {
		return true
	}
	return responsesProbeBodyHasFunctionCall(body)
}

func responsesProbeBodyHasFunctionCall(body []byte) bool {
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		return false
	}
	for _, item := range output.Array() {
		if strings.TrimSpace(item.Get("type").String()) == "function_call" {
			return true
		}
	}
	return false
}

func responsesProbeVerdictIsConclusive(status int, body []byte) bool {
	if status < 200 || status >= 300 {
		return true
	}
	switch strings.TrimSpace(gjson.GetBytes(body, "status").String()) {
	case "failed":
		return false
	case "incomplete":
		return strings.TrimSpace(gjson.GetBytes(body, "incomplete_details.reason").String()) != "max_output_tokens"
	default:
		return true
	}
}
