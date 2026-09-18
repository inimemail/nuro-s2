package service

// 国产供应商 Anthropic 协议账号的 Responses 入站反向路径。
//
// 客户端说 OpenAI Responses（/v1/responses，Codex 等）、上游是供应商原生
// Anthropic 端点（api_protocol=anthropic）时的交叉组合：请求 Responses→Anthropic
// 单次转换，响应 Anthropic 事件→Responses 事件转换。转换链与 Anthropic 平台的
// gateway_forward_as_responses.go 完全一致（复用同一组 apicompat 状态机），仅上游
// 发送/错误处理对齐 OpenAI 网关语义（模型映射、failover、transport error）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// forwardResponsesViaNativeAnthropic serves OpenAI /v1/responses clients through
// a CN provider's native Anthropic endpoint.
//
// Conversion chain:
//
//	Request:  Responses → Anthropic (single conversion)
//	Response: Anthropic events → Responses events (stream state machine)
func (s *OpenAIGatewayService) forwardResponsesViaNativeAnthropic(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	// 1. Lower Codex client-side tools to function tools understood by Anthropic.
	adaptedBody, clientToolMapping, err := adaptResponsesClientToolsForAnthropic(body)
	if err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to adapt request tools")
		return nil, fmt.Errorf("adapt responses client tools: %w", err)
	}

	// 2. Parse Responses request
	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(adaptedBody, &responsesReq); err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	originalModel := responsesReq.Model
	if strings.TrimSpace(originalModel) == "" {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	clientStream := responsesReq.Stream

	// 3. Convert Responses → Anthropic
	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(&responsesReq)
	if err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to convert request")
		return nil, fmt.Errorf("convert responses to anthropic: %w", err)
	}

	// 4. Model mapping（OpenAI 网关统一入口的映射语义）
	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	anthropicReq.Model = upstreamModel

	reasoningEffort := ExtractResponsesReasoningEffortFromBody(body)
	reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, body, billingModel)

	// 5. Force upstream streaming（客户端原始终决定响应格式；
	// 上游恒为流式，非流式由缓冲路径组装）。
	anthropicReq.Stream = true
	reqStream := true

	logger.L().Debug("openai responses: forwarding via native anthropic endpoint",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("client_stream", clientStream),
	)

	anthropicBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}

	// 与 /v1/messages 直通路径相同的 pre-filter。
	anthropicBody = StripEmptyTextBlocks(anthropicBody)
	anthropicBody = FilterWebSearchHistoryBlocks(anthropicBody, upstreamModel)
	anthropicBody = enforceCacheControlLimit(anthropicBody)

	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return nil, fmt.Errorf("account %d missing api_key", account.ID)
	}
	targetURL, err := s.nativeAnthropicTargetURL(account)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, reqStream)
	upstreamReq, _, err := s.buildNativeAnthropicUpstreamRequest(upstreamCtx, c, account, anthropicBody, apiKey, targetURL)
	releaseUpstreamCtx()
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	resp, err := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, true)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, upstreamMsg := s.readOpenAIUpstreamError(resp)
		if foErr := s.failoverOpenAIUpstreamHTTPError(ctx, c, account, resp, respBody, upstreamMsg, upstreamModel); foErr != nil {
			return nil, foErr
		}
		writeResponsesError(c, mapUpstreamStatusCode(resp.StatusCode), "server_error", upstreamMsg)
		return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
	}

	if clientStream {
		return s.handleResponsesStreamingFromNativeAnthropic(resp, c, originalModel, billingModel, upstreamModel, reasoningEffort, startTime, clientToolMapping)
	}
	return s.handleResponsesBufferedFromNativeAnthropic(resp, c, originalModel, billingModel, upstreamModel, reasoningEffort, startTime, clientToolMapping)
}

// handleResponsesBufferedFromNativeAnthropic reads Anthropic SSE events, assembles
// the full response, then converts Anthropic → Responses.
func (s *OpenAIGatewayService) handleResponsesBufferedFromNativeAnthropic(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	startTime time.Time,
	clientToolMapping apicompat.ResponsesClientToolMapping,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	finalResp, usage, err := s.readNativeAnthropicResponse(resp)
	if err != nil {
		writeResponsesError(c, http.StatusBadGateway, "upstream_error", "Upstream stream failed or ended before completion")
		return nil, err
	}

	responsesResp := apicompat.AnthropicToResponsesResponse(finalResp)
	responsesResp.Model = originalModel

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	// 非流式响应必须是 application/json（上游被强制流式，透传头会污染）。
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	if respBytes, err := json.Marshal(responsesResp); err == nil {
		respBytes = reverseToolNamesIfPresent(c, respBytes)
		respBytes, _, err = apicompat.RestoreResponsesClientToolPayload(respBytes, clientToolMapping)
		if err != nil {
			return nil, fmt.Errorf("restore responses client tools: %w", err)
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", respBytes)
	} else {
		c.JSON(http.StatusOK, responsesResp)
	}

	return &OpenAIForwardResult{
		RequestID:        requestID,
		Usage:            claudeUsageToOpenAIUsage(&usage),
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: "/v1/messages",
		ReasoningEffort:  reasoningEffort,
		Stream:           false,
		Duration:         time.Since(startTime),
	}, nil
}

// handleResponsesStreamingFromNativeAnthropic reads Anthropic SSE events, converts
// each to Responses SSE events, and writes them to the client.
func (s *OpenAIGatewayService) handleResponsesStreamingFromNativeAnthropic(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	startTime time.Time,
	clientToolMapping apicompat.ResponsesClientToolMapping,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	state := apicompat.NewAnthropicEventToResponsesState()
	state.Model = originalModel
	clientToolRestorer := apicompat.NewResponsesClientToolStreamRestorer(clientToolMapping)

	var usage ClaudeUsage
	var firstTokenMs *int
	firstChunk := true
	clientDisconnected := false
	lastSequenceNumber := -1

	resultWithUsage := func() *OpenAIForwardResult {
		return &OpenAIForwardResult{
			RequestID:        requestID,
			Usage:            claudeUsageToOpenAIUsage(&usage),
			Model:            originalModel,
			BillingModel:     billingModel,
			UpstreamModel:    upstreamModel,
			UpstreamEndpoint: "/v1/messages",
			ReasoningEffort:  reasoningEffort,
			Stream:           true,
			Duration:         time.Since(startTime),
			FirstTokenMs:     firstTokenMs,
			ClientDisconnect: clientDisconnected,
		}
	}

	// 与 CC 姊妹路径（handleCCStreamingFromNativeAnthropic.writeChunk）同语义：
	// 客户端断开后不再写出，但继续排水上游至流自然结束——Anthropic 的最终
	// output_tokens 只在末尾 message_delta 携带，提前退出会把整段生成记成 ~1
	// token，payg 上游照常计费而平台漏记。状态机照常推进以保证 finalize 一致。
	processAnthropicEvent := func(event *apicompat.AnthropicStreamEvent) error {
		if firstChunk {
			firstChunk = false
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}

		if event.Type == "message_delta" && event.Usage != nil {
			mergeAnthropicUsage(&usage, *event.Usage)
		}
		if event.Type == "message_start" && event.Message != nil {
			mergeAnthropicUsage(&usage, event.Message.Usage)
		}

		events := apicompat.AnthropicEventToResponsesEvents(event, state)
		if clientDisconnected {
			return nil
		}
		for _, evt := range events {
			payload, err := json.Marshal(evt)
			if err != nil {
				return fmt.Errorf("encode converted Anthropic event: %w", err)
			}
			payload = reverseToolNamesIfPresent(c, payload)
			payloads, _, err := clientToolRestorer.RestoreEvent(payload)
			if err != nil {
				return fmt.Errorf("restore converted Anthropic tools: %w", err)
			}
			for _, restored := range payloads {
				eventType := gjson.GetBytes(restored, "type").String()
				if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, restored); err != nil {
					clientDisconnected = true
					return nil
				}
				lastSequenceNumber = int(gjson.GetBytes(restored, "sequence_number").Int())
			}
		}
		if len(events) > 0 {
			c.Writer.Flush()
		}
		return nil
	}

	err := s.scanNativeAnthropicEvents(resp, processAnthropicEvent)
	if err != nil {
		if !clientDisconnected {
			if !c.Writer.Written() {
				writeResponsesError(c, http.StatusBadGateway, "upstream_error", "Upstream stream failed or ended before completion")
			} else {
				payload, _ := json.Marshal(gin.H{"type": "response.failed", "sequence_number": lastSequenceNumber + 1, "response": gin.H{"id": state.ResponseID, "model": originalModel, "object": "response", "created_at": state.Created, "status": "failed", "output": []any{}, "error": gin.H{"code": "upstream_error", "message": "Upstream stream failed or ended before completion"}}})
				_, _ = fmt.Fprintf(c.Writer, "event: response.failed\ndata: %s\n\n", payload)
				c.Writer.Flush()
			}
		}
		result := resultWithUsage()
		result.TerminalEventType = "error"
		return result, err
	}

	// The reader requires message_stop; its converted terminal event has
	// already been emitted and must not be synthesized again after EOF.
	return resultWithUsage(), nil
}
