package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func (s *OpenAIGatewayService) ForwardGrokVoice(ctx context.Context, c *gin.Context, account *Account, endpoint string, body []byte, contentType string) (*OpenAIForwardResult, error) {
	if account == nil || account.Platform != PlatformGrok {
		return nil, fmt.Errorf("grok account is required")
	}
	endpoint = strings.Trim(strings.TrimSpace(endpoint), "/")
	baseEndpoint := strings.Split(endpoint, "/")[0]
	if baseEndpoint != "tts" && baseEndpoint != "stt" && baseEndpoint != "custom-voices" {
		return nil, fmt.Errorf("unsupported grok voice endpoint")
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	targetURL, err := buildGrokVoiceURL(account, s.cfg, endpoint)
	if err != nil {
		return nil, err
	}
	method := http.MethodPost
	if c != nil && c.Request != nil && c.Request.Method != "" {
		method = c.Request.Method
	}
	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()
	req, err := http.NewRequestWithContext(upstreamCtx, method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, audio/*")
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	account.ApplyHeaderOverrides(req.Header)
	proxyURL := resolveAccountProxyURL(account)
	started := time.Now()
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(started).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return s.handleGrokMediaErrorResponse(ctx, resp, c, account, resp.Header.Get("x-request-id"))
	}
	data, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	writeGrokMediaResponse(c, resp, data, s.responseHeaderFilter)
	return &OpenAIForwardResult{
		RequestID:     StableGrokAudioBillingRequestID(firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id"))),
		Model:         baseEndpoint,
		UpstreamModel: baseEndpoint,
		Duration:      time.Since(started),
		AudioUsage:    estimateGrokVoiceAudioUsage(baseEndpoint, body, data, time.Since(started)),
	}, nil
}

func (s *OpenAIGatewayService) ProxyGrokRealtime(ctx context.Context, client *coderws.Conn, account *Account, token, model string) (bool, error) {
	base, err := buildGrokVoiceURL(account, s.cfg, "realtime")
	if err != nil {
		return false, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return false, err
	}
	u.Scheme = "wss"
	u.RawQuery = "model=" + url.QueryEscape(firstNonEmpty(model, "grok-voice-latest"))
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	account.ApplyHeaderOverrides(headers)
	upstream, _, _, err := s.getOpenAIWSPassthroughDialer().Dial(ctx, u.String(), headers, resolveAccountProxyURL(account))
	if err != nil {
		return false, err
	}
	defer func() { _ = upstream.Close() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	var audioObserved atomic.Bool
	go func() {
		for {
			msg, readErr := upstream.ReadMessage(ctx)
			if readErr != nil {
				errCh <- readErr
				return
			}
			if grokRealtimeEventHasAudio(msg) {
				audioObserved.Store(true)
			}
			if writeErr := client.Write(ctx, coderws.MessageText, msg); writeErr != nil {
				errCh <- writeErr
				return
			}
		}
	}()
	go func() {
		for {
			kind, msg, readErr := client.Read(ctx)
			if readErr != nil {
				errCh <- readErr
				return
			}
			if kind != coderws.MessageText && kind != coderws.MessageBinary {
				continue
			}
			if grokRealtimeEventHasAudio(msg) {
				audioObserved.Store(true)
			}
			var raw json.RawMessage
			if err := json.Unmarshal(msg, &raw); err != nil {
				errCh <- err
				return
			}
			if err := upstream.WriteJSON(ctx, raw); err != nil {
				errCh <- err
				return
			}
		}
	}()
	err = <-errCh
	return audioObserved.Load(), err
}

func grokRealtimeEventHasAudio(msg []byte) bool {
	if !gjson.ValidBytes(msg) {
		return false
	}
	eventType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(msg, "type").String()))
	if !strings.Contains(eventType, "audio") || strings.Contains(eventType, "transcript") {
		return false
	}
	for _, path := range []string{"audio", "delta", "data"} {
		value := gjson.GetBytes(msg, path)
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			return true
		}
	}
	return false
}

func estimateGrokVoiceAudioUsage(endpoint string, reqBody, respBody []byte, elapsed time.Duration) *AudioUsage {
	switch endpoint {
	case "tts":
		text := firstNonEmpty(gjson.GetBytes(reqBody, "input").String(), gjson.GetBytes(reqBody, "text").String(), gjson.GetBytes(reqBody, "prompt").String())
		chars := len([]rune(text))
		if chars == 0 {
			chars = len(reqBody)
		}
		if chars > 0 {
			return &AudioUsage{Mode: "tts", DurationOrUnits: float64(chars) / 1_000_000}
		}
	case "stt":
		secs := gjson.GetBytes(respBody, "duration").Float()
		if secs <= 0 {
			secs = gjson.GetBytes(respBody, "duration_seconds").Float()
		}
		if floor := float64(len(reqBody)) / 16000; floor > secs {
			secs = floor
		}
		if secs <= 0 {
			secs = elapsed.Seconds()
		}
		if secs > 0 {
			return &AudioUsage{Mode: "stt", DurationOrUnits: secs / 3600}
		}
	}
	return nil
}

func StableGrokAudioBillingRequestID(id string) string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "grok_audio:") {
		return id
	}
	if id == "" {
		id = generateRequestID()
	}
	return "grok_audio:" + id
}

func StableGrokRealtimeBillingRequestID(id string) string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "grok_realtime:") {
		return id
	}
	if id == "" {
		id = generateRequestID()
	}
	return "grok_realtime:" + id
}

func (s *OpenAIGatewayService) ForwardGrokWebSearch(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	return s.forwardGrokNativeSearch(ctx, c, account, body, "web_search")
}

func (s *OpenAIGatewayService) ForwardGrokXSearch(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	return s.forwardGrokNativeSearch(ctx, c, account, body, "x_search")
}

func (s *OpenAIGatewayService) forwardGrokNativeSearch(ctx context.Context, c *gin.Context, account *Account, body []byte, toolType string) (*OpenAIForwardResult, error) {
	if account == nil || account.Platform != PlatformGrok {
		return nil, fmt.Errorf("grok account is required")
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	targetURL, err := buildGrokResponsesURL(account, s.cfg)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(gjson.GetBytes(body, "query").String())
	if query == "" {
		query = strings.TrimSpace(gjson.GetBytes(body, "input").String())
	}
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	searchModel := DefaultGrokSearchBillingModel
	if upstreamModel, ok := ResolvedUpstreamModelFromContext(ctx); ok {
		searchModel = upstreamModel
	}
	tool := map[string]any{"type": toolType}
	if toolType == "x_search" {
		for _, key := range []string{"allowed_x_handles", "excluded_x_handles", "from_date", "to_date", "enable_image_understanding", "enable_video_understanding"} {
			if value := gjson.GetBytes(body, key); value.Exists() {
				tool[key] = value.Value()
			}
		}
	}
	include := toolType + "_call.action.sources"
	searchPayload := map[string]any{
		"model":   searchModel,
		"input":   query,
		"tools":   []map[string]any{tool},
		"include": []string{include},
		"store":   false,
		"stream":  false,
	}
	if toolType == "x_search" {
		searchPayload["tool_choice"] = "required"
	}
	searchBody, _ := json.Marshal(searchPayload)
	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()
	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, bytes.NewReader(searchBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	account.ApplyHeaderOverrides(req.Header)
	started := time.Now()
	resp, err := s.httpUpstream.Do(req, resolveAccountProxyURL(account), account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(started).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return s.handleGrokMediaErrorResponse(ctx, resp, c, account, resp.Header.Get("x-request-id"))
	}
	data, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	count := countGrokNativeSearchCallsFromJSONBytes(data)
	if count <= 0 {
		count = 1
	}
	requestID := firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id"))
	if toolType == "x_search" {
		if requestID == "" {
			requestID = generateRequestID()
		}
		requestID = "x_search:" + requestID
	} else {
		requestID = StableGrokWebSearchBillingRequestID(requestID)
	}
	return &OpenAIForwardResult{
		RequestID:     requestID,
		Model:         DefaultGrokSearchBillingModel,
		UpstreamModel: searchModel,
		Duration:      time.Since(started),
		SearchCount:   count,
		ResponseBody:  data,
	}, nil
}

const DefaultGrokSearchBillingModel = "grok-4.5"

func StableGrokWebSearchBillingRequestID(id string) string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "web_search:") {
		return id
	}
	if id == "" {
		id = generateRequestID()
	}
	return "web_search:" + id
}
