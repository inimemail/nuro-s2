package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type GrokMediaEndpoint string

const (
	GrokMediaEndpointImagesGenerations GrokMediaEndpoint = "images_generations"
	GrokMediaEndpointImagesEdits       GrokMediaEndpoint = "images_edits"
	GrokMediaEndpointVideosGenerations GrokMediaEndpoint = "videos_generations"
	GrokMediaEndpointVideosEdits       GrokMediaEndpoint = "videos_edits"
	GrokMediaEndpointVideosExtensions  GrokMediaEndpoint = "videos_extensions"
	GrokMediaEndpointVideoStatus       GrokMediaEndpoint = "video_status"
	GrokMediaEndpointVideoContent      GrokMediaEndpoint = "video_content"
)

func (e GrokMediaEndpoint) RequiresRequestBody() bool {
	return !e.IsVideoLookupRequest()
}

func (e GrokMediaEndpoint) IsVideoLookupRequest() bool {
	return e == GrokMediaEndpointVideoStatus || e == GrokMediaEndpointVideoContent
}

func (e GrokMediaEndpoint) IsGenerationRequest() bool {
	switch e {
	case GrokMediaEndpointImagesGenerations, GrokMediaEndpointImagesEdits, GrokMediaEndpointVideosGenerations, GrokMediaEndpointVideosEdits, GrokMediaEndpointVideosExtensions:
		return true
	default:
		return false
	}
}

func (e GrokMediaEndpoint) IsVideoMutationRequest() bool {
	switch e {
	case GrokMediaEndpointVideosGenerations, GrokMediaEndpointVideosEdits, GrokMediaEndpointVideosExtensions:
		return true
	default:
		return false
	}
}

type GrokMediaRequestInfo struct {
	Model           string
	Prompt          string
	N               int
	Size            string
	SizeTier        string
	Resolution      string
	DurationSeconds int
	InputImageURLs  []string
	MaskImageURL    string
	Uploads         []OpenAIImagesUpload
	MaskUpload      *OpenAIImagesUpload
}

func (r GrokMediaRequestInfo) ModerationBody() []byte {
	payload := map[string]any{}
	if prompt := strings.TrimSpace(r.Prompt); prompt != "" {
		payload["prompt"] = prompt
	}

	images := make([]map[string]string, 0, len(r.InputImageURLs)+len(r.Uploads)+1)
	for _, imageURL := range r.InputImageURLs {
		if imageURL = strings.TrimSpace(imageURL); imageURL != "" {
			images = append(images, map[string]string{"image_url": imageURL})
		}
	}
	for _, upload := range r.Uploads {
		if dataURL := upload.ModerationDataURL(); dataURL != "" {
			images = append(images, map[string]string{"image_url": dataURL})
		}
	}
	if maskURL := strings.TrimSpace(r.MaskImageURL); maskURL != "" {
		images = append(images, map[string]string{"image_url": maskURL})
	}
	if r.MaskUpload != nil {
		if dataURL := r.MaskUpload.ModerationDataURL(); dataURL != "" {
			images = append(images, map[string]string{"image_url": dataURL})
		}
	}
	if len(images) > 0 {
		payload["images"] = images
	}
	if len(payload) == 0 {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return body
}

func (e GrokMediaEndpoint) httpMethod() string {
	if e == GrokMediaEndpointVideoStatus {
		return http.MethodGet
	}
	return http.MethodPost
}

func ExtractGrokMediaModel(contentType string, body []byte) string {
	return ParseGrokMediaRequest(contentType, body).Model
}

func ParseGrokMediaRequest(contentType string, body []byte) GrokMediaRequestInfo {
	info := GrokMediaRequestInfo{N: 1}
	if gjson.ValidBytes(body) {
		parseGrokMediaJSONRequest(body, &info)
	} else {
		parseGrokMediaMultipartRequest(contentType, body, &info)
	}
	info.Model = strings.TrimSpace(info.Model)
	info.Prompt = strings.TrimSpace(info.Prompt)
	info.Size = strings.TrimSpace(info.Size)
	info.SizeTier = NormalizeImageBillingTierOrDefault(info.Size)
	info.Resolution = NormalizeVideoBillingResolutionOrDefault(info.Resolution)
	info.DurationSeconds = NormalizeVideoBillingDurationSecondsOrDefault(info.DurationSeconds)
	if info.N <= 0 {
		info.N = 1
	}
	return info
}

func parseGrokMediaJSONRequest(body []byte, info *GrokMediaRequestInfo) {
	if info == nil {
		return
	}
	info.Model = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	info.Prompt = strings.TrimSpace(gjson.GetBytes(body, "prompt").String())
	info.Size = strings.TrimSpace(gjson.GetBytes(body, "size").String())
	info.Resolution = strings.TrimSpace(gjson.GetBytes(body, "resolution").String())
	if duration := gjson.GetBytes(body, "duration"); duration.Exists() && duration.Type == gjson.Number {
		info.DurationSeconds = int(duration.Int())
	}
	if n := gjson.GetBytes(body, "n"); n.Exists() && n.Type == gjson.Number {
		info.N = int(n.Int())
	}
	appendJSONImageURLs := func(value gjson.Result) {
		if !value.Exists() {
			return
		}
		switch {
		case value.IsArray():
			for _, item := range value.Array() {
				if imageURL := grokMediaJSONImageURL(item); imageURL != "" {
					info.InputImageURLs = append(info.InputImageURLs, imageURL)
					continue
				}
				if item.Type == gjson.String {
					if imageURL := strings.TrimSpace(item.String()); imageURL != "" {
						info.InputImageURLs = append(info.InputImageURLs, imageURL)
					}
				}
			}
		default:
			if imageURL := grokMediaJSONImageURL(value); imageURL != "" {
				info.InputImageURLs = append(info.InputImageURLs, imageURL)
				return
			}
			if value.Type == gjson.String {
				if imageURL := strings.TrimSpace(value.String()); imageURL != "" {
					info.InputImageURLs = append(info.InputImageURLs, imageURL)
				}
			}
		}
	}
	appendJSONImageURLs(gjson.GetBytes(body, "image"))
	appendJSONImageURLs(gjson.GetBytes(body, "images"))
	appendJSONImageURLs(gjson.GetBytes(body, "reference_images"))
	info.MaskImageURL = grokMediaJSONImageURL(gjson.GetBytes(body, "mask"))
}

// grokMediaJSONImageURL accepts both the current xAI {url: ...} shape and the
// historical OpenAI-compatible {image_url: ...} shape. Prefer url when both
// are present so a client can safely send a canonical override.
func grokMediaJSONImageURL(value gjson.Result) string {
	if value.Type == gjson.String {
		return strings.TrimSpace(value.String())
	}
	if imageURL := strings.TrimSpace(value.Get("url").String()); imageURL != "" {
		return imageURL
	}
	return strings.TrimSpace(value.Get("image_url").String())
}

func parseGrokMediaMultipartRequest(contentType string, body []byte, info *GrokMediaRequestInfo) {
	if info == nil {
		return
	}
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		return
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
		name := strings.TrimSpace(part.FormName())
		if name == "" {
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, openAIImageMaxUploadPartSize))
		_ = part.Close()
		if err != nil {
			return
		}
		fileName := strings.TrimSpace(part.FileName())
		partContentType := strings.TrimSpace(part.Header.Get("Content-Type"))
		if fileName != "" {
			upload := OpenAIImagesUpload{
				FieldName:   name,
				FileName:    fileName,
				ContentType: partContentType,
				Data:        data,
			}
			if name == "mask" {
				info.MaskUpload = &upload
				continue
			}
			if name == "image" || strings.HasPrefix(name, "image[") {
				info.Uploads = append(info.Uploads, upload)
			}
			continue
		}

		value := strings.TrimSpace(string(data))
		switch name {
		case "model":
			info.Model = value
		case "prompt":
			info.Prompt = value
		case "size":
			info.Size = value
		case "resolution":
			info.Resolution = value
		case "duration":
			if duration, err := strconv.Atoi(value); err == nil {
				info.DurationSeconds = duration
			}
		case "n":
			if n, err := strconv.Atoi(value); err == nil {
				info.N = n
			}
		case "image", "image_url":
			if value != "" {
				info.InputImageURLs = append(info.InputImageURLs, value)
			}
		case "mask", "mask_image_url":
			info.MaskImageURL = value
		}
	}
}

var (
	ErrGrokMediaVideoRequestNotFound    = errors.New("grok video request not found")
	ErrGrokMediaVideoBindingUnavailable = errors.New("grok video request binding unavailable")
)

type GrokMediaVideoBinding struct {
	GroupID  *int64
	UserID   int64
	APIKeyID int64
}

func GrokMediaVideoRequestSessionHash(requestID string, userID, apiKeyID int64) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || userID <= 0 || apiKeyID <= 0 {
		return ""
	}
	return "grok-video:" + DeriveSessionHashFromSeed(fmt.Sprintf("%d:%d:%s", userID, apiKeyID, requestID))
}

func (s *OpenAIGatewayService) BindGrokMediaVideoRequestAccount(ctx context.Context, groupID *int64, requestID string, userID, apiKeyID, accountID int64) error {
	if s == nil || s.cache == nil {
		return ErrGrokMediaVideoBindingUnavailable
	}
	sessionHash := GrokMediaVideoRequestSessionHash(requestID, userID, apiKeyID)
	if sessionHash == "" || accountID <= 0 {
		return ErrGrokMediaVideoBindingUnavailable
	}
	return s.BindStickySession(ctx, groupID, sessionHash, accountID)
}

// SelectBoundGrokMediaVideoRequestAccount resolves an asynchronous video task
// to the account that created it. A bound task must never fall through to the
// normal priority scheduler because request IDs are account-local.
func (s *OpenAIGatewayService) SelectBoundGrokMediaVideoRequestAccount(ctx context.Context, groupID *int64, requestID string, userID, apiKeyID int64) (*AccountSelectionResult, bool, error) {
	if s == nil || s.cache == nil {
		return nil, true, ErrGrokMediaVideoBindingUnavailable
	}
	sessionHash := GrokMediaVideoRequestSessionHash(requestID, userID, apiKeyID)
	if sessionHash == "" {
		return nil, true, ErrGrokMediaVideoRequestNotFound
	}
	accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, true, ErrGrokMediaVideoRequestNotFound
		}
		return nil, true, fmt.Errorf("%w: resolve grok video account binding: %v", ErrGrokMediaVideoBindingUnavailable, err)
	}
	if accountID <= 0 {
		return nil, true, ErrGrokMediaVideoRequestNotFound
	}

	account, err := s.getSchedulableAccount(ctx, accountID)
	if err != nil || account == nil {
		return nil, true, ErrNoAvailableAccounts
	}
	if account.Platform != PlatformGrok || !account.IsSchedulable() || !s.latestOpenAIAccountMatchesGroup(ctx, account, groupID) {
		return nil, true, ErrNoAvailableAccounts
	}

	acquired, err := s.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency, account.Platform)
	if err != nil {
		return nil, true, err
	}
	if acquired != nil && acquired.Acquired {
		_ = s.refreshStickySessionTTL(ctx, groupID, sessionHash, s.openAIStickySessionTTLForHash(sessionHash, s.openAIWSSessionStickyTTL()))
		selection, selectionErr := s.newAcquiredSelectionResult(ctx, account, acquired.ReleaseFunc)
		return selection, true, selectionErr
	}

	cfg := s.schedulingConfig()
	selection, selectionErr := s.newSelectionResult(ctx, account, false, nil, &AccountWaitPlan{
		AccountID:      account.ID,
		MaxConcurrency: account.Concurrency,
		Timeout:        cfg.StickySessionWaitTimeout,
		MaxWaiting:     cfg.StickySessionMaxWaiting,
	})
	return selection, true, selectionErr
}

func (e GrokMediaEndpoint) upstreamURL(baseURL, requestID string) (string, error) {
	switch e {
	case GrokMediaEndpointImagesGenerations:
		return xai.BuildImagesGenerationsURL(baseURL)
	case GrokMediaEndpointImagesEdits:
		return xai.BuildImagesEditsURL(baseURL)
	case GrokMediaEndpointVideosGenerations:
		return xai.BuildVideosGenerationsURL(baseURL)
	case GrokMediaEndpointVideosEdits:
		return xai.BuildVideosEditsURL(baseURL)
	case GrokMediaEndpointVideosExtensions:
		return xai.BuildVideosExtensionsURL(baseURL)
	case GrokMediaEndpointVideoStatus:
		return xai.BuildVideoURL(baseURL, requestID)
	case GrokMediaEndpointVideoContent:
		videoURL, err := xai.BuildVideoURL(baseURL, requestID)
		if err != nil {
			return "", err
		}
		return videoURL + "/content", nil
	default:
		return "", fmt.Errorf("unsupported grok media endpoint: %s", e)
	}
}

func (s *OpenAIGatewayService) ForwardGrokMedia(ctx context.Context, c *gin.Context, account *Account, endpoint GrokMediaEndpoint, requestID string, body []byte, contentType string) (*OpenAIForwardResult, error) {
	return s.forwardGrokMedia(ctx, c, account, endpoint, requestID, body, contentType, nil)
}

func (s *OpenAIGatewayService) ForwardGrokMediaWithVideoBinding(ctx context.Context, c *gin.Context, account *Account, endpoint GrokMediaEndpoint, requestID string, body []byte, contentType string, binding GrokMediaVideoBinding) (*OpenAIForwardResult, error) {
	bindVideo := func(responseID string) error {
		return s.BindGrokMediaVideoRequestAccount(ctx, binding.GroupID, responseID, binding.UserID, binding.APIKeyID, account.ID)
	}
	return s.forwardGrokMedia(ctx, c, account, endpoint, requestID, body, contentType, bindVideo)
}

func (s *OpenAIGatewayService) forwardGrokMedia(ctx context.Context, c *gin.Context, account *Account, endpoint GrokMediaEndpoint, requestID string, body []byte, contentType string, bindVideo func(string) error) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	if account == nil {
		return nil, fmt.Errorf("grok account is required")
	}
	if account.Platform != PlatformGrok {
		return nil, fmt.Errorf("account platform %s is not supported for grok media", account.Platform)
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	if endpoint == GrokMediaEndpointVideoContent {
		return s.forwardGrokMediaVideoContent(ctx, c, account, token, requestID, startTime)
	}
	targetURL, err := buildGrokMediaURL(account, s.cfg, endpoint, requestID)
	if err != nil {
		return nil, err
	}
	body, contentType, err = prepareGrokMediaForwardBody(endpoint, body, contentType)
	if err != nil {
		return nil, err
	}
	body, contentType, err = normalizeGrokMediaForwardBody(endpoint, body, contentType)
	if err != nil {
		return nil, err
	}
	requestInfo := ParseGrokMediaRequest(contentType, body)
	body, contentType, err = sanitizeGrokMediaForwardBody(endpoint, body, contentType)
	if err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if endpoint.RequiresRequestBody() {
		bodyReader = bytes.NewReader(body)
	}
	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	upstreamReq, err := http.NewRequestWithContext(upstreamCtx, endpoint.httpMethod(), targetURL, bodyReader)
	if err != nil {
		return nil, err
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+token)
	upstreamReq.Header.Set("Accept", "application/json")
	applyGrokOAuthIdentityHeaders(upstreamReq.Header, targetURL, account.IsGrokOAuth())
	if endpoint.RequiresRequestBody() {
		contentType = strings.TrimSpace(contentType)
		if contentType == "" {
			contentType = "application/json"
		}
		upstreamReq.Header.Set("Content-Type", contentType)
	}
	account.ApplyHeaderOverrides(upstreamReq.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	upstreamStart := time.Now()
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	requestIDHeader := firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id"))
	requestModel := requestInfo.Model
	if resp.StatusCode >= 400 {
		return s.handleGrokMediaErrorResponse(ctx, resp, c, account, requestIDHeader)
	}

	s.updateGrokUsageFromResponse(ctx, account, resp.Header, resp.StatusCode)
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	terminalEventType := ""
	if status, terminal := grokMediaUnsuccessfulTerminalStatus(endpoint, respBody); terminal {
		respBody = sanitizeGrokMediaUnsuccessfulTerminalResponse(respBody, status)
		terminalEventType = "grok_media." + status
	}
	if !grokMediaSuccessResponseIsValid(endpoint, respBody) {
		return nil, &UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: respBody,
			Message:      safeUpstreamErrorMessage,
		}
	}
	if endpoint == GrokMediaEndpointVideoStatus {
		respBody = rewriteGrokMediaVideoContentURLs(respBody, requestID, grokMediaContentProxyURL(c, requestID))
	}
	usage := grokMediaUsageFromResponse(endpoint, requestInfo, respBody)
	if endpoint.IsVideoMutationRequest() && usage.ResponseID != "" && bindVideo != nil {
		if err := bindVideo(usage.ResponseID); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrGrokMediaVideoBindingUnavailable, err)
		}
	}
	writeGrokMediaResponse(c, resp, respBody, s.responseHeaderFilter)
	return &OpenAIForwardResult{
		RequestID:            requestIDHeader,
		ResponseID:           usage.ResponseID,
		Usage:                usage.Usage,
		Model:                requestModel,
		BillingModel:         requestModel,
		UpstreamModel:        requestModel,
		ResponseHeaders:      resp.Header.Clone(),
		Duration:             time.Since(startTime),
		TerminalEventType:    terminalEventType,
		ImageCount:           usage.ImageCount,
		ImageSize:            usage.ImageSize,
		ImageInputSize:       usage.ImageInputSize,
		ImageOutputSizes:     usage.ImageOutputSizes,
		VideoCount:           usage.VideoCount,
		VideoResolution:      usage.VideoResolution,
		VideoDurationSeconds: usage.VideoDurationSeconds,
	}, nil
}

func (s *OpenAIGatewayService) forwardGrokMediaVideoContent(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	token, requestID string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	statusURL, err := buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideoStatus, requestID)
	if err != nil {
		return nil, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	statusReq, err := http.NewRequestWithContext(
		WithHTTPUpstreamRedirectsDisabled(upstreamCtx),
		http.MethodGet,
		statusURL,
		nil,
	)
	if err != nil {
		return nil, err
	}
	statusReq.Header.Set("Authorization", "Bearer "+token)
	statusReq.Header.Set("Accept", "application/json")
	applyGrokOAuthIdentityHeaders(statusReq.Header, statusURL, account.IsGrokOAuth())
	account.ApplyHeaderOverrides(statusReq.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	upstreamStart := time.Now()
	statusResp, err := s.httpUpstream.Do(statusReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	statusRequestID := firstNonEmpty(statusResp.Header.Get("x-request-id"), statusResp.Header.Get("xai-request-id"))
	if statusResp.StatusCode >= 300 {
		defer func() { _ = statusResp.Body.Close() }()
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
		if statusResp.StatusCode < 400 {
			return nil, errors.New("grok media status redirect is not allowed")
		}
		return s.handleGrokMediaErrorResponse(ctx, statusResp, c, account, statusRequestID)
	}
	statusBody, err := ReadUpstreamResponseBody(statusResp.Body, s.cfg, c, openAITooLargeError)
	_ = statusResp.Body.Close()
	if err != nil {
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
		return nil, err
	}

	contentURL, err := grokMediaSignedVideoContentURL(statusBody, requestID)
	if err != nil {
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
		return nil, err
	}
	signedContent := contentURL != ""
	if !signedContent {
		contentURL, err = buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideoContent, requestID)
		if err != nil {
			return nil, err
		}
	}

	contentReq, err := http.NewRequestWithContext(
		WithHTTPUpstreamRedirectsDisabled(upstreamCtx),
		http.MethodGet,
		contentURL,
		nil,
	)
	if err != nil {
		return nil, err
	}
	contentReq.Header.Set("Accept", "*/*")
	if c != nil {
		if rangeHeader := strings.TrimSpace(c.GetHeader("Range")); rangeHeader != "" {
			contentReq.Header.Set("Range", rangeHeader)
		}
	}
	if !signedContent {
		contentReq.Header.Set("Authorization", "Bearer "+token)
		applyGrokOAuthIdentityHeaders(contentReq.Header, contentURL, account.IsGrokOAuth())
		account.ApplyHeaderOverrides(contentReq.Header)
	}

	contentResp, err := s.httpUpstream.Do(contentReq, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = contentResp.Body.Close() }()
	contentRequestID := firstNonEmpty(contentResp.Header.Get("x-request-id"), contentResp.Header.Get("xai-request-id"), statusRequestID)
	if contentResp.StatusCode >= 300 && contentResp.StatusCode < 400 {
		return nil, errors.New("grok media content redirect is not allowed")
	}
	if contentResp.StatusCode >= 400 && contentResp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		return s.handleGrokMediaErrorResponse(ctx, contentResp, c, account, contentRequestID)
	}
	if err := writeGrokMediaContentResponse(c, contentResp); err != nil {
		return nil, err
	}
	return &OpenAIForwardResult{
		RequestID:       contentRequestID,
		ResponseHeaders: contentResp.Header.Clone(),
		Duration:        time.Since(startTime),
	}, nil
}

func grokMediaSignedVideoContentURL(body []byte, requestID string) (string, error) {
	rawURL := strings.TrimSpace(gjson.GetBytes(body, "video.url").String())
	if rawURL == "" || isGrokMediaVideoContentURL(rawURL, requestID) {
		return "", nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") ||
		!strings.EqualFold(parsed.Hostname(), "vidgen.x.ai") ||
		(parsed.Port() != "" && parsed.Port() != "443") || parsed.User != nil {
		return "", errors.New("grok media status returned an unsupported video content URL")
	}
	return parsed.String(), nil
}

func isGrokCLIProxyTarget(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	return err == nil && strings.EqualFold(parsed.Hostname(), "cli-chat-proxy.grok.com")
}

func grokMediaSuccessResponseIsValid(endpoint GrokMediaEndpoint, body []byte) bool {
	if _, terminal := grokMediaUnsuccessfulTerminalStatus(endpoint, body); terminal {
		return true
	}
	if openAIPassthroughResponseIsUnsafe(body) {
		return false
	}
	switch endpoint {
	case GrokMediaEndpointImagesGenerations, GrokMediaEndpointImagesEdits:
		return countOpenAIResponseImageOutputsFromJSONBytes(body) > 0
	case GrokMediaEndpointVideosGenerations, GrokMediaEndpointVideosEdits, GrokMediaEndpointVideosExtensions:
		return extractGrokMediaVideoRequestID(body) != ""
	case GrokMediaEndpointVideoStatus:
		status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "status").String()))
		switch status {
		case "pending", "queued", "processing", "in_progress", "completed", "done", "expired", "failed", "cancelled", "canceled":
			return true
		default:
			return extractGrokMediaVideoRequestID(body) != "" && gjson.GetBytes(body, "video").IsObject()
		}
	default:
		return false
	}
}

func grokMediaUnsuccessfulTerminalStatus(endpoint GrokMediaEndpoint, body []byte) (string, bool) {
	if endpoint != GrokMediaEndpointVideoStatus || !gjson.ValidBytes(body) {
		return "", false
	}
	status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "status").String()))
	switch status {
	case "failed", "cancelled", "canceled":
		return status, true
	default:
		return "", false
	}
}

func sanitizeGrokMediaUnsuccessfulTerminalResponse(body []byte, status string) []byte {
	payload := map[string]any{"status": status}
	var source map[string]any
	if err := json.Unmarshal(body, &source); err == nil {
		for _, key := range []string{"request_id", "id"} {
			if value, ok := source[key].(string); ok && openAIErrorIDIsSafe(value) {
				payload[key] = value
			}
		}
		for _, key := range []string{"created_at", "updated_at", "expires_at", "progress"} {
			if value, ok := source[key].(float64); ok {
				payload[key] = value
			}
		}
	}
	if _, hasRequestID := payload["request_id"]; !hasRequestID {
		if _, hasID := payload["id"]; !hasID {
			if requestID := extractGrokMediaVideoRequestID(body); openAIErrorIDIsSafe(requestID) {
				payload["request_id"] = requestID
			}
		}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"status":"failed"}`)
	}
	return out
}

func prepareGrokMediaForwardBody(endpoint GrokMediaEndpoint, body []byte, contentType string) ([]byte, string, error) {
	if endpoint != GrokMediaEndpointImagesEdits || gjson.ValidBytes(body) {
		return body, contentType, nil
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		return body, contentType, nil
	}

	info := ParseGrokMediaRequest(contentType, body)
	payload := make(map[string]any)
	if info.Model != "" {
		payload["model"] = info.Model
	}
	if info.Prompt != "" {
		payload["prompt"] = info.Prompt
	}
	if info.N > 1 {
		payload["n"] = info.N
	}
	if info.Size != "" {
		payload["size"] = info.Size
	}

	images := make([]map[string]string, 0, len(info.InputImageURLs)+len(info.Uploads))
	for _, imageURL := range info.InputImageURLs {
		if imageURL = strings.TrimSpace(imageURL); imageURL != "" {
			images = append(images, map[string]string{"url": imageURL})
		}
	}
	for _, upload := range info.Uploads {
		dataURL, err := openAIImageUploadToDataURL(upload)
		if err != nil {
			return nil, "", err
		}
		images = append(images, map[string]string{"url": dataURL})
	}
	if len(images) > 0 {
		payload["image"] = images[0]
		if len(images) > 1 {
			payload["images"] = images
		}
	}

	maskImageURL := strings.TrimSpace(info.MaskImageURL)
	if info.MaskUpload != nil {
		dataURL, err := openAIImageUploadToDataURL(*info.MaskUpload)
		if err != nil {
			return nil, "", err
		}
		maskImageURL = dataURL
	}
	if maskImageURL != "" {
		payload["mask"] = map[string]string{"url": maskImageURL}
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	return out, "application/json", nil
}

func normalizeGrokMediaForwardBody(endpoint GrokMediaEndpoint, body []byte, contentType string) ([]byte, string, error) {
	if !endpoint.RequiresRequestBody() || !gjson.ValidBytes(body) {
		return body, contentType, nil
	}
	var imageFields []string
	switch endpoint {
	case GrokMediaEndpointImagesEdits:
		imageFields = []string{"image", "images", "mask"}
	case GrokMediaEndpointVideosGenerations:
		imageFields = []string{"image", "images", "reference_images"}
	}
	var err error
	body, err = canonicalizeGrokMediaImageURLFields(body, imageFields...)
	if err != nil {
		return nil, "", err
	}
	info := ParseGrokMediaRequest(contentType, body)
	upstreamModel := normalizeGrokMediaModelForEndpoint(endpoint, info.Model, info.HasInputImage())
	if upstreamModel == "" || upstreamModel == info.Model {
		return body, contentType, nil
	}
	out, err := sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, "", fmt.Errorf("rewrite grok media model: %w", err)
	}
	return out, contentType, nil
}

func canonicalizeGrokMediaImageURLFields(body []byte, fields ...string) ([]byte, error) {
	out := body
	for _, field := range fields {
		value := gjson.GetBytes(out, field)
		if !value.Exists() {
			continue
		}
		if value.IsArray() {
			for index := range value.Array() {
				var err error
				out, err = canonicalizeGrokMediaImageURLObject(out, fmt.Sprintf("%s.%d", field, index))
				if err != nil {
					return nil, err
				}
			}
			continue
		}
		var err error
		out, err = canonicalizeGrokMediaImageURLObject(out, field)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func canonicalizeGrokMediaImageURLObject(body []byte, path string) ([]byte, error) {
	legacyPath := path + ".image_url"
	legacy := gjson.GetBytes(body, legacyPath)
	if !legacy.Exists() {
		return body, nil
	}
	out := body
	if strings.TrimSpace(gjson.GetBytes(out, path+".url").String()) == "" {
		var err error
		out, err = sjson.SetBytes(out, path+".url", legacy.Value())
		if err != nil {
			return nil, fmt.Errorf("normalize grok media image url: %w", err)
		}
	}
	out, err := sjson.DeleteBytes(out, legacyPath)
	if err != nil {
		return nil, fmt.Errorf("remove legacy grok media image url: %w", err)
	}
	return out, nil
}

func sanitizeGrokMediaForwardBody(endpoint GrokMediaEndpoint, body []byte, contentType string) ([]byte, string, error) {
	if !endpoint.RequiresRequestBody() || !gjson.ValidBytes(body) {
		return body, contentType, nil
	}
	switch endpoint {
	case GrokMediaEndpointImagesGenerations, GrokMediaEndpointImagesEdits:
		if !gjson.GetBytes(body, "size").Exists() {
			return body, contentType, nil
		}
		out, err := sjson.DeleteBytes(body, "size")
		if err != nil {
			return nil, "", fmt.Errorf("sanitize grok media size: %w", err)
		}
		return out, contentType, nil
	default:
		return body, contentType, nil
	}
}

func (r GrokMediaRequestInfo) HasInputImage() bool {
	return len(r.InputImageURLs) > 0 || len(r.Uploads) > 0
}

func normalizeGrokMediaModelForEndpoint(endpoint GrokMediaEndpoint, model string, hasInputImage bool) string {
	model = strings.TrimSpace(model)
	switch endpoint {
	case GrokMediaEndpointImagesGenerations, GrokMediaEndpointImagesEdits:
		if model == "grok-imagine" {
			return "grok-imagine-image-quality"
		}
	case GrokMediaEndpointVideosGenerations:
		if model == "grok-imagine-video-1.5" && !hasInputImage {
			return "grok-imagine-video"
		}
	}
	return model
}

type grokMediaUsageMetadata struct {
	ResponseID           string
	Usage                OpenAIUsage
	ImageCount           int
	ImageSize            string
	ImageInputSize       string
	ImageOutputSizes     []string
	VideoCount           int
	VideoResolution      string
	VideoDurationSeconds int
}

func grokMediaUsageFromResponse(endpoint GrokMediaEndpoint, requestInfo GrokMediaRequestInfo, responseBody []byte) grokMediaUsageMetadata {
	usage, _ := extractOpenAIUsageFromJSONBytes(responseBody)
	meta := grokMediaUsageMetadata{Usage: usage}
	switch endpoint {
	case GrokMediaEndpointImagesGenerations, GrokMediaEndpointImagesEdits:
		imageCount := countOpenAIResponseImageOutputsFromJSONBytes(responseBody)
		if imageCount <= 0 {
			imageCount = requestInfo.N
		}
		if imageCount <= 0 {
			imageCount = 1
		}
		meta.ImageCount = imageCount
		meta.ImageSize = requestInfo.SizeTier
		meta.ImageInputSize = requestInfo.Size
		meta.ImageOutputSizes = collectOpenAIResponseImageOutputSizesFromJSONBytes(responseBody)
	case GrokMediaEndpointVideosGenerations, GrokMediaEndpointVideosEdits, GrokMediaEndpointVideosExtensions:
		meta.ResponseID = extractGrokMediaVideoRequestID(responseBody)
		meta.VideoCount = 1
		meta.VideoResolution = requestInfo.Resolution
		meta.VideoDurationSeconds = requestInfo.DurationSeconds
		meta.ImageCount = 1
	}
	return meta
}

func extractGrokMediaVideoRequestID(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	for _, path := range []string{"request_id", "id", "data.request_id", "data.id", "video.request_id", "video.id"} {
		if id := strings.TrimSpace(gjson.GetBytes(body, path).String()); id != "" {
			return id
		}
	}
	return ""
}

func (s *OpenAIGatewayService) handleGrokMediaErrorResponse(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, requestIDHeader string) (*OpenAIForwardResult, error) {
	body, readErr := readUpstreamResponseBodyLimited(resp.Body, 2<<20)
	if readErr != nil {
		body = nil
	}
	s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body)
	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if upstreamMsg == "" {
		upstreamMsg = safeUpstreamErrorMessage
	}
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)

	if status, errType, errMsg, matched := applyErrorPassthroughRule(c, account.Platform, resp.StatusCode, body, http.StatusBadGateway, "upstream_error", "Upstream request failed"); matched {
		MarkResponseCommitted(c)
		writeGrokMediaErrorResponse(c, status, errType, errMsg)
		return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
	}
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  requestIDHeader,
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		writeGrokMediaErrorResponse(c, http.StatusInternalServerError, "upstream_error", "Upstream gateway error")
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	kind := "http_error"
	if s.shouldFailoverUpstreamError(resp.StatusCode) {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  requestIDHeader,
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if kind == "failover" {
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           body,
			RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	MarkResponseCommitted(c)
	writeGrokMediaErrorResponse(c, resp.StatusCode, grokMediaErrorType(resp.StatusCode), safeUpstreamErrorMessage)
	return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
}

func grokMediaErrorType(statusCode int) string {
	switch statusCode {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "upstream_error"
	}
}

func writeGrokMediaErrorResponse(c *gin.Context, statusCode int, errType, message string) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    strings.TrimSpace(errType),
			"message": strings.TrimSpace(message),
		},
	})
}

func writeGrokMediaResponse(c *gin.Context, resp *http.Response, body []byte, filter *responseheaders.CompiledHeaderFilter) {
	if c == nil || resp == nil {
		return
	}
	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, filter)
	contentType := responseheaders.SafeContentType(resp.Header.Get("Content-Type"), "application/json")
	c.Data(resp.StatusCode, contentType, body)
}

func writeGrokMediaContentResponse(c *gin.Context, resp *http.Response) error {
	if c == nil || resp == nil || resp.Body == nil {
		return errors.New("grok media content response is incomplete")
	}
	for _, name := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := strings.TrimSpace(resp.Header.Get(name)); value != "" {
			c.Header(name, value)
		}
	}
	contentType := safeGrokMediaVideoContentType(resp.Header.Get("Content-Type"))
	c.Header("Content-Type", contentType)
	c.Header("X-Content-Type-Options", "nosniff")
	if strings.TrimSpace(resp.Header.Get("Content-Disposition")) != "" {
		c.Header("Content-Disposition", `attachment; filename="`+safeGrokMediaVideoFilename(contentType)+`"`)
	}
	if c.Writer.Header().Get("Content-Length") == "" && resp.ContentLength >= 0 {
		c.Header("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	c.Status(resp.StatusCode)
	MarkResponseCommitted(c)
	_, err := io.Copy(c.Writer, resp.Body)
	return err
}

func safeGrokMediaVideoContentType(raw string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(raw))
	if err != nil {
		return "application/octet-stream"
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if strings.HasPrefix(mediaType, "video/") || mediaType == "application/octet-stream" {
		return mediaType
	}
	return "application/octet-stream"
}

func safeGrokMediaVideoFilename(contentType string) string {
	switch contentType {
	case "video/mp4":
		return "video.mp4"
	case "video/webm":
		return "video.webm"
	case "video/quicktime":
		return "video.mov"
	case "video/mpeg":
		return "video.mpeg"
	default:
		return "video.bin"
	}
}

func rewriteGrokMediaVideoContentURLs(body []byte, requestID, proxyURL string) []byte {
	if len(body) == 0 || strings.TrimSpace(requestID) == "" || strings.TrimSpace(proxyURL) == "" || !gjson.ValidBytes(body) {
		return body
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return body
	}
	changed := rewriteGrokMediaKnownVideoURL(&value, proxyURL)
	if rewriteGrokMediaVideoContentURLValue(&value, requestID, proxyURL) {
		changed = true
	}
	if !changed {
		return body
	}
	rewritten, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return rewritten
}

func rewriteGrokMediaKnownVideoURL(value *any, proxyURL string) bool {
	root, ok := (*value).(map[string]any)
	if !ok {
		return false
	}
	video, ok := root["video"].(map[string]any)
	if !ok {
		return false
	}
	rawURL, ok := video["url"].(string)
	if !ok || strings.TrimSpace(rawURL) == "" {
		return false
	}
	video["url"] = proxyURL
	return true
}

func rewriteGrokMediaVideoContentURLValue(value *any, requestID, proxyURL string) bool {
	if value == nil {
		return false
	}
	switch typed := (*value).(type) {
	case map[string]any:
		changed := false
		for key, child := range typed {
			childValue := child
			if rewriteGrokMediaVideoContentURLValue(&childValue, requestID, proxyURL) {
				typed[key] = childValue
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for index, child := range typed {
			childValue := child
			if rewriteGrokMediaVideoContentURLValue(&childValue, requestID, proxyURL) {
				typed[index] = childValue
				changed = true
			}
		}
		return changed
	case string:
		if isGrokMediaVideoContentURL(typed, requestID) {
			*value = proxyURL
			return true
		}
	}
	return false
}

func isGrokMediaVideoContentURL(rawURL, requestID string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Path == "" {
		return false
	}
	segments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(segments) < 3 {
		return false
	}
	decodedID, err := url.PathUnescape(segments[len(segments)-2])
	if err != nil {
		return false
	}
	return segments[len(segments)-3] == "videos" &&
		decodedID == strings.Trim(requestID, "/") &&
		segments[len(segments)-1] == "content"
}

func grokMediaContentProxyURL(c *gin.Context, requestID string) string {
	if c == nil || c.Request == nil || c.Request.URL == nil || strings.TrimSpace(requestID) == "" {
		return ""
	}
	prefix := ""
	if strings.HasPrefix(c.Request.URL.Path, "/v1/") {
		prefix = "/v1"
	}
	return prefix + "/videos/" + url.PathEscape(strings.Trim(requestID, "/")) + "/content"
}
