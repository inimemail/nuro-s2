package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type AccountTestMedia struct {
	ImageDataURL string
	AudioDataURL string
}

func (s *AccountTestService) testGrokAccountConnection(c *gin.Context, account *Account, modelID string, prompt string, mode string, media ...AccountTestMedia) error {
	ctx := c.Request.Context()
	if s.httpUpstream == nil {
		return s.sendErrorAndEnd(c, "HTTP upstream not configured")
	}
	model := strings.TrimSpace(modelID)
	if len(prompt) > 8<<20 {
		return s.sendErrorAndEnd(c, "test media exceeds 8 MB limit")
	}
	autoMode := mode == "" || mode == "default"
	if autoMode {
		mode = "text"
	}
	switch mode {
	case "text", "chat", "search":
		model = firstNonEmpty(model, xai.DefaultTextModel)
	case "image":
		model = firstNonEmpty(model, xai.DefaultImagineImageQualityModel)
	case "video":
		model = firstNonEmpty(model, xai.DefaultImagineVideoModel)
	case "tts", "stt":
		model = ""
	case "realtime":
		model = "grok-voice-latest"
	default:
		return s.sendErrorAndEnd(c, "Unsupported Grok test mode")
	}
	if model != "" && mode != "realtime" {
		model = account.GetMappedModel(model)
		kind := grokTestModelKind(model)
		if autoMode {
			mode = kind
		}
		if (mode == "image" && kind != "image") || (mode == "video" && kind != "video") ||
			((mode == "text" || mode == "chat" || mode == "search") && kind != "text") {
			return s.sendErrorAndEnd(c, "Selected model does not support this Grok test mode")
		}
	}
	opts := AccountTestMedia{}
	if len(media) > 0 {
		opts = media[0]
	}
	// Accept old clients while keeping uploaded bytes separate from text.
	if strings.HasPrefix(prompt, "data:image/") && (mode == "image" || mode == "video") {
		opts.ImageDataURL, prompt = firstNonEmpty(opts.ImageDataURL, prompt), ""
	}
	if strings.HasPrefix(prompt, "data:audio/") && mode == "stt" {
		opts.AudioDataURL, prompt = firstNonEmpty(opts.AudioDataURL, prompt), ""
	}
	if opts.ImageDataURL != "" {
		if _, _, err := decodeGrokTestMedia(opts.ImageDataURL, "image/"); err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
	}
	var token string
	var err error
	switch account.Type {
	case AccountTypeOAuth:
		if s.grokTokenProvider == nil {
			return s.sendErrorAndEnd(c, "Grok token provider not configured")
		}
		token, err = s.grokTokenProvider.GetAccessTokenForManualTest(ctx, account)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to get Grok access token")
		}
	case AccountTypeAPIKey:
		token = strings.TrimSpace(account.GetGrokAccessToken())
		if token == "" {
			return s.sendErrorAndEnd(c, "Grok API key is missing")
		}
	default:
		return s.sendErrorAndEnd(c, fmt.Sprintf("Unsupported Grok account type: %s", account.Type))
	}
	if mode != "text" && mode != "chat" {
		return s.testGrokSpecialMode(c, account, model, prompt, mode, token, opts)
	}
	var body []byte
	var url string
	if mode == "chat" {
		body, err = json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": firstNonEmptyTestPrompt(prompt, "hi")}}, "stream": true})
		if err == nil {
			url, err = buildGrokChatCompletionsURL(account, s.cfg)
		}
	} else {
		body, err = json.Marshal(map[string]any{"model": model, "input": []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": firstNonEmptyTestPrompt(prompt, "hi")}}}}, "stream": true})
		if err == nil {
			url, err = buildGrokResponsesURL(account, s.cfg)
		}
	}
	if err != nil {
		return s.sendErrorAndEnd(c, "Could not build Grok test request; check the base URL and URL security policy")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	applyGrokOAuthIdentityHeaders(req.Header, url, account.IsGrokOAuth())
	account.ApplyHeaderOverrides(req.Header)
	if account.Proxy != nil {
		resp, err := s.httpUpstream.DoWithTLS(req, account.Proxy.URL(), account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
		return s.finishGrokTest(c, account, resp, err, model, mode)
	}
	resp, err := s.httpUpstream.DoWithTLS(req, "", account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	return s.finishGrokTest(c, account, resp, err, model, mode)
}

// testGrokSpecialMode exercises the same native endpoints used by the gateway,
// while keeping account-test traffic out of the normal pool/cooldown path.
func (s *AccountTestService) testGrokSpecialMode(c *gin.Context, account *Account, model, prompt, mode, token string, opts AccountTestMedia) error {
	var endpoint GrokMediaEndpoint
	var target string
	var payload []byte
	var buildErr error
	contentType := "application/json"
	text := firstNonEmptyTestPrompt(prompt, "hi")
	switch mode {
	case "image":
		endpoint = GrokMediaEndpointImagesGenerations
		if opts.ImageDataURL != "" {
			endpoint = GrokMediaEndpointImagesEdits
		}
		target, buildErr = buildGrokMediaURL(account, s.cfg, endpoint, "")
		if buildErr != nil {
			return s.sendErrorAndEnd(c, "endpoint unsupported")
		}
		model = normalizeGrokMediaModelForEndpoint(endpoint, model, opts.ImageDataURL != "")
		imagePayload := map[string]any{"model": model, "prompt": firstNonEmpty(prompt, "Draw a small orange cat."), "n": 1, "response_format": "b64_json"}
		if opts.ImageDataURL != "" {
			imagePayload["image"] = map[string]string{"url": opts.ImageDataURL, "type": "image_url"}
		}
		payload, _ = json.Marshal(imagePayload)
	case "video":
		endpoint = GrokMediaEndpointVideosGenerations
		target, buildErr = buildGrokMediaURL(account, s.cfg, endpoint, "")
		if buildErr != nil {
			return s.sendErrorAndEnd(c, "endpoint unsupported")
		}
		model = normalizeGrokMediaModelForEndpoint(endpoint, model, opts.ImageDataURL != "")
		videoPayload := map[string]any{"model": model, "prompt": firstNonEmpty(prompt, "A small orange cat walking in a garden."), "duration": 6, "resolution": "480p"}
		if opts.ImageDataURL != "" {
			videoPayload["image"] = map[string]string{"url": opts.ImageDataURL, "type": "image_url"}
		}
		payload, _ = json.Marshal(videoPayload)
	case "search":
		target, buildErr = buildGrokResponsesURL(account, s.cfg)
		if buildErr != nil {
			return s.sendErrorAndEnd(c, "endpoint unsupported")
		}
		payload, _ = json.Marshal(map[string]any{"model": firstNonEmpty(model, "grok-4.5"), "input": text, "tools": []map[string]any{{"type": "web_search"}}, "stream": false})
	case "tts", "stt":
		endpointName := mode
		target, buildErr = buildGrokVoiceURL(account, s.cfg, endpointName)
		if buildErr != nil {
			return s.sendErrorAndEnd(c, "endpoint unsupported")
		}
		if mode == "tts" {
			payload, _ = json.Marshal(map[string]any{"text": text, "language": "en", "voice_id": "Ara"})
		} else {
			audio, mimeType, err := decodeGrokTestMedia(opts.AudioDataURL, "audio/")
			if err != nil {
				return s.sendErrorAndEnd(c, err.Error())
			}
			var buf bytes.Buffer
			writer := multipart.NewWriter(&buf)
			part, err := writer.CreateFormFile("file", grokTestAudioFilename(mimeType))
			if err != nil {
				return s.sendErrorAndEnd(c, "Could not build audio test")
			}
			_, _ = part.Write(audio)
			_ = writer.WriteField("model", "grok-stt")
			_ = writer.WriteField("language", "en")
			_ = writer.Close()
			payload, contentType = buf.Bytes(), writer.FormDataContentType()
		}
	case "realtime":
		return s.testGrokRealtime(c, account, model, token)
	default:
		return s.sendErrorAndEnd(c, "Unsupported Grok test mode")
	}
	if target == "" {
		return s.sendErrorAndEnd(c, "Grok endpoint is not configured")
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok test request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json, audio/*")
	applyGrokOAuthIdentityHeaders(req.Header, target, account.IsGrokOAuth())
	account.ApplyHeaderOverrides(req.Header)
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil || resp == nil || resp.Body == nil {
		return s.sendErrorAndEnd(c, "service unavailable")
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if readErr != nil || len(data) > 8<<20 {
		return s.sendErrorAndEnd(c, "Grok response could not be read")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.sendErrorAndEnd(c, grokTestErrorCategory(resp.StatusCode, data))
	}
	if mode != "tts" && (!gjson.ValidBytes(data) || openAIJSONFieldHasNonNullValue(data, "error")) {
		return s.sendErrorAndEnd(c, "Grok returned an invalid or unsuccessful test response")
	}
	status := strings.ToLower(firstNonEmpty(gjson.GetBytes(data, "status").String(), gjson.GetBytes(data, "state").String()))
	if status == "failed" || status == "error" || status == "cancelled" || status == "canceled" || status == "incomplete" {
		return s.sendErrorAndEnd(c, "Grok test response did not complete successfully")
	}
	if mode == "search" && (status != "completed" || !gjson.GetBytes(data, "output").IsArray()) {
		return s.sendErrorAndEnd(c, "Grok search returned no completed response")
	}
	if mode == "stt" && gjson.GetBytes(data, "text").Type != gjson.String {
		return s.sendErrorAndEnd(c, "Grok returned no transcription")
	}
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	if mode == "image" {
		if imageURL := gjson.GetBytes(data, "data.0.url").String(); imageURL != "" {
			s.sendEvent(c, TestEvent{Type: "image", ImageURL: imageURL, MimeType: "image/*"})
		} else if encoded := gjson.GetBytes(data, "data.0.b64_json").String(); encoded != "" {
			// The upstream value is already base64; only wrap it as a preview URL.
			s.sendEvent(c, TestEvent{Type: "image", ImageURL: "data:image/png;base64," + encoded, MimeType: "image/png"})
		} else {
			return s.sendErrorAndEnd(c, "Grok returned no image")
		}
	} else if mode == "video" {
		videoURL := firstNonEmpty(gjson.GetBytes(data, "video.url").String(), gjson.GetBytes(data, "url").String())
		if videoURL == "" {
			requestID := firstNonEmpty(gjson.GetBytes(data, "id").String(), gjson.GetBytes(data, "request_id").String())
			if requestID != "" {
				videoURL = s.pollGrokTestVideo(c, account, requestID, token)
			}
		}
		if videoURL == "" {
			return s.sendErrorAndEnd(c, "Video generation failed or did not complete within the test timeout")
		}
		s.sendEvent(c, TestEvent{Type: "video", VideoURL: videoURL, MimeType: "video/mp4"})
	} else if mode == "tts" {
		// Voice APIs may return either a URL or base64 audio. Forward only the
		// media value so the UI can provide an in-browser preview without
		// exposing the complete upstream response.
		audioURL := firstNonEmpty(gjson.GetBytes(data, "audio_url").String(), gjson.GetBytes(data, "data").String())
		mimeType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
		if strings.HasPrefix(mimeType, "audio/") && len(data) > 0 {
			s.sendEvent(c, TestEvent{Type: "audio", AudioURL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), MimeType: mimeType})
		} else if audioURL != "" && !openAIJSONFieldHasNonNullValue(data, "error") {
			if !strings.HasPrefix(audioURL, "data:") && !strings.HasPrefix(audioURL, "http://") && !strings.HasPrefix(audioURL, "https://") {
				audioURL = "data:audio/mpeg;base64," + audioURL
			}
			s.sendEvent(c, TestEvent{Type: "audio", AudioURL: audioURL, MimeType: "audio/mpeg"})
		} else {
			return s.sendErrorAndEnd(c, "Grok returned no audio")
		}
	} else if mode == "stt" {
		s.sendEvent(c, TestEvent{Type: "content", Text: firstNonEmptyTestPrompt(gjson.GetBytes(data, "text").String(), string(data))})
	} else {
		s.sendEvent(c, TestEvent{Type: "content", Text: string(data)})
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) testGrokRealtime(c *gin.Context, account *Account, model, token string) error {
	target, err := buildGrokVoiceURL(account, s.cfg, "realtime")
	if err != nil {
		return s.sendErrorAndEnd(c, "realtime endpoint unsupported")
	}
	u, err := url.Parse(target)
	if err != nil {
		return s.sendErrorAndEnd(c, "invalid realtime endpoint")
	}
	u.Scheme = "wss"
	q := u.Query()
	q.Set("model", firstNonEmpty(model, "grok-voice-latest"))
	u.RawQuery = q.Encode()
	dialCtx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	applyGrokOAuthIdentityHeaders(headers, u.String(), account.IsGrokOAuth())
	account.ApplyHeaderOverrides(headers)
	dialOptions := &coderws.DialOptions{HTTPHeader: headers}
	if proxyURL := resolveAccountProxyURL(account); proxyURL != "" {
		_, parsedProxy, parseErr := proxyurl.Parse(proxyURL)
		if parseErr != nil {
			return s.sendErrorAndEnd(c, "invalid proxy configuration")
		}
		transport := &http.Transport{TLSHandshakeTimeout: 10 * time.Second}
		if proxyErr := proxyutil.ConfigureTransportProxy(transport, parsedProxy); proxyErr != nil {
			return s.sendErrorAndEnd(c, "invalid proxy configuration")
		}
		dialOptions.HTTPClient = &http.Client{Transport: transport}
	}
	conn, _, err := coderws.Dial(dialCtx, u.String(), dialOptions)
	if err != nil {
		return s.sendErrorAndEnd(c, grokTestErrorCategory(http.StatusBadGateway, []byte(err.Error())))
	}
	defer conn.CloseNow()
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Flush()
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	for _, event := range []map[string]any{
		{"type": "session.update", "session": map[string]any{"modalities": []string{"text", "audio"}, "model": firstNonEmpty(model, "grok-voice-latest")}},
		{"type": "conversation.item.create", "item": map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}}},
		{"type": "response.create"},
	} {
		payload, _ := json.Marshal(event)
		if err := conn.Write(dialCtx, coderws.MessageText, payload); err != nil {
			return s.sendErrorAndEnd(c, "realtime request failed")
		}
	}
	for {
		_, payload, readErr := conn.Read(dialCtx)
		if readErr != nil {
			return s.sendErrorAndEnd(c, "Grok realtime stream ended before response.done")
		}
		var envelope map[string]any
		if json.Unmarshal(payload, &envelope) == nil {
			typ, _ := envelope["type"].(string)
			if typ != "" {
				s.sendEvent(c, TestEvent{Type: "status", Text: typ})
			}
			if delta, ok := envelope["delta"].(string); ok && delta != "" && (strings.Contains(typ, "text") || strings.Contains(typ, "transcript")) {
				s.sendEvent(c, TestEvent{Type: "content", Text: delta})
			}
			if typ == "error" || typ == "response.failed" {
				return s.sendErrorAndEnd(c, "Grok realtime test failed")
			}
			if typ == "response.done" {
				if status := gjson.GetBytes(payload, "response.status").String(); status != "" && status != "completed" {
					return s.sendErrorAndEnd(c, "Grok realtime response did not complete")
				}
				s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
				return nil
			}
		}
	}
}

func (s *AccountTestService) pollGrokTestVideo(c *gin.Context, account *Account, requestID, token string) string {
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			select {
			case <-c.Request.Context().Done():
				return ""
			case <-time.After(1500 * time.Millisecond):
			}
		}
		target, err := buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideoStatus, requestID)
		if err != nil {
			return ""
		}
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, target, nil)
		if err != nil {
			return ""
		}
		req.Header.Set("Authorization", "Bearer "+token)
		applyGrokOAuthIdentityHeaders(req.Header, target, account.IsGrokOAuth())
		account.ApplyHeaderOverrides(req.Header)
		proxyURL := resolveAccountProxyURL(account)
		resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
		if err != nil || resp == nil || resp.Body == nil {
			return ""
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
		resp.Body.Close()
		if readErr != nil || len(body) > 2<<20 || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return ""
		}
		status := strings.ToLower(firstNonEmpty(gjson.GetBytes(body, "status").String(), gjson.GetBytes(body, "state").String()))
		if status == "failed" || status == "error" || status == "cancelled" || status == "canceled" || status == "expired" {
			return ""
		}
		if videoURL := firstNonEmpty(gjson.GetBytes(body, "video.url").String(), gjson.GetBytes(body, "url").String(), gjson.GetBytes(body, "video_url").String()); videoURL != "" {
			return videoURL
		}
	}
	return ""
}

func firstNonEmptyTestPrompt(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func (s *AccountTestService) finishGrokTest(c *gin.Context, account *Account, resp *http.Response, err error, model, mode string) error {
	if err != nil {
		return s.sendErrorAndEnd(c, "service unavailable")
	}
	if resp == nil || resp.Body == nil {
		return s.sendErrorAndEnd(c, "Grok returned no response")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return s.sendErrorAndEnd(c, grokTestErrorCategory(resp.StatusCode, body))
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	if mode == "chat" {
		return s.processOpenAIChatCompletionsStream(c, resp.Body)
	}
	return s.processOpenAIStream(c, resp.Body)
}

func grokTestModelKind(model string) string {
	id := strings.ToLower(xai.StripGrokProviderPrefix(model))
	if strings.HasPrefix(id, "grok-imagine-video") || strings.HasPrefix(id, "grok-video") {
		return "video"
	}
	if isGrokImageGenerationModel(id) {
		return "image"
	}
	return "text"
}

// Bound encoded input before decoding, and allow only uploaded media, never a
// URL fetched with account credentials. The limit is on decoded file bytes.
func decodeGrokTestMedia(value, prefix string) ([]byte, string, error) {
	const maxBytes = 8 << 20
	if len(value) > base64.StdEncoding.EncodedLen(maxBytes)+128 {
		return nil, "", fmt.Errorf("test media exceeds 8 MB limit")
	}
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(header, "data:"+prefix) || !strings.HasSuffix(header, ";base64") {
		return nil, "", fmt.Errorf("upload a valid %s file for this test", strings.TrimSuffix(prefix, "/"))
	}
	mimeType := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	if strings.ContainsAny(mimeType, ";\r\n") {
		return nil, "", fmt.Errorf("invalid test media type")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 || len(decoded) > maxBytes {
		return nil, "", fmt.Errorf("invalid or oversized test media")
	}
	return decoded, mimeType, nil
}

func grokTestAudioFilename(mimeType string) string {
	switch mimeType {
	case "audio/mpeg", "audio/mp3":
		return "probe.mp3"
	case "audio/ogg":
		return "probe.ogg"
	case "audio/webm":
		return "probe.webm"
	case "audio/mp4", "audio/x-m4a":
		return "probe.m4a"
	case "audio/flac":
		return "probe.flac"
	default:
		return "probe.wav"
	}
}

func grokTestErrorCategory(status int, body []byte) string {
	lower := strings.ToLower(string(body))
	switch status {
	case http.StatusUnauthorized:
		return "token invalid"
	case http.StatusForbidden:
		return "permission denied"
	case http.StatusNotFound:
		if strings.Contains(lower, "model") {
			return "model not found"
		}
		return "endpoint unsupported"
	case http.StatusPaymentRequired:
		return "entitlement unavailable"
	case http.StatusTooManyRequests:
		return "rate limited"
	case http.StatusBadRequest:
		return "unsupported request field"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "service unavailable"
	default:
		if status >= 500 {
			return "service unavailable"
		}
		return "upstream request rejected"
	}
}
