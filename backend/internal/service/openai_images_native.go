package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type OpenAIImagesSVGOptions struct {
	Model     string `json:"model"`
	Reasoning *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
}

func requiresCodexDirectImages(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-image-2.6-flare", "gpt-image-2.6-sunburst":
		return true
	}
	return false
}
func parseOpenAIImages26Field(name, raw string, req *OpenAIImagesRequest) error {
	switch name {
	case "complexity":
		if gjson.Parse(raw).Type != gjson.String {
			return fmt.Errorf("invalid complexity")
		}
		if err := json.Unmarshal([]byte(raw), &req.Complexity); err != nil {
			return fmt.Errorf("invalid complexity")
		}
	case "frame_count", "frame_duration_ms":
		var n int
		if err := json.Unmarshal([]byte(raw), &n); err != nil || n <= 0 || int64(n) > math.MaxInt32 {
			return fmt.Errorf("%s must be a positive 32-bit integer", name)
		}
		if name == "frame_count" {
			req.FrameCount = &n
		} else {
			req.FrameDurationMS = &n
		}
	case "svg":
		var svg OpenAIImagesSVGOptions
		d := json.NewDecoder(strings.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&svg); err != nil || strings.TrimSpace(svg.Model) == "" {
			return fmt.Errorf("invalid svg author options")
		}
		// Multipart fields do not pass through the outer JSON validator. Require
		// EOF as well so a second value or trailing garbage cannot be ignored.
		if err := d.Decode(new(any)); err != io.EOF {
			return fmt.Errorf("invalid svg author options")
		}
		if svg.Reasoning != nil {
			switch svg.Reasoning.Effort {
			case "", "none", "low", "medium", "high", "xhigh", "max":
			default:
				return fmt.Errorf("invalid svg reasoning effort")
			}
		}
		req.SVG = &svg
	}
	req.HasNativeOptions = true
	return nil
}
func validateOpenAIImages26Options(req *OpenAIImagesRequest) error {
	switch req.Complexity {
	case "", "low", "medium", "high":
	default:
		return fmt.Errorf("complexity must be low, medium or high")
	}
	return nil
}
func buildOpenAIImagesOAuthPayload(parsed *OpenAIImagesRequest, model string) ([]byte, string, error) {
	if parsed == nil {
		return nil, "", fmt.Errorf("parsed image request is required")
	}
	if !requiresCodexDirectImages(model) {
		body, err := buildOpenAIImagesResponsesRequest(parsed, model)
		return body, chatgptCodexURL, err
	}
	if parsed == nil || strings.TrimSpace(parsed.Prompt) == "" {
		return nil, "", fmt.Errorf("prompt is required")
	}
	if err := validateOpenAIImages26Options(parsed); err != nil {
		return nil, "", err
	}
	if err := reserveOpenAIImagesPayload(parsed); err != nil {
		return nil, "", err
	}
	payload := map[string]any{"model": model, "prompt": parsed.Prompt}
	if !parsed.Multipart {
		if original := gjson.GetBytes(parsed.Body, "prompt"); original.Type == gjson.String {
			payload["prompt"] = original.String()
		}
	}
	for key, value := range map[string]string{"size": parsed.Size, "quality": parsed.Quality, "complexity": parsed.Complexity, "background": parsed.Background, "output_format": parsed.OutputFormat, "moderation": parsed.Moderation, "input_fidelity": parsed.InputFidelity, "style": parsed.Style} {
		if value != "" {
			payload[key] = value
		}
	}
	if parsed.N > 1 {
		payload["n"] = parsed.N
	}
	if parsed.Stream {
		payload["stream"] = true
	}
	if parsed.OutputCompression != nil {
		payload["output_compression"] = *parsed.OutputCompression
	}
	if parsed.PartialImages != nil {
		payload["partial_images"] = *parsed.PartialImages
	}
	if parsed.FrameCount != nil {
		payload["frame_count"] = *parsed.FrameCount
	}
	if parsed.FrameDurationMS != nil {
		payload["frame_duration_ms"] = *parsed.FrameDurationMS
	}
	if parsed.SVG != nil {
		payload["svg"] = parsed.SVG
	}
	endpoint := "/images/generations"
	if parsed.IsEdits() {
		endpoint = "/images/edits"
		images := make([]map[string]string, 0, len(parsed.Uploads)+len(parsed.InputImageURLs))
		for _, url := range parsed.InputImageURLs {
			images = append(images, map[string]string{"image_url": url})
		}
		for _, upload := range parsed.Uploads {
			url, err := openAIImageUploadToDataURL(upload, parsed.bodyReservation)
			if err != nil {
				return nil, "", err
			}
			images = append(images, map[string]string{"image_url": url})
		}
		if len(images) == 0 {
			return nil, "", fmt.Errorf("image input is required")
		}
		payload["images"] = images
		for key, input := range map[string]struct {
			url    string
			upload *OpenAIImagesUpload
		}{"mask": {parsed.MaskImageURL, parsed.MaskUpload}, "markup": {parsed.MarkupImageURL, parsed.MarkupUpload}} {
			url := input.url
			if input.upload != nil {
				var err error
				url, err = openAIImageUploadToDataURL(*input.upload, parsed.bodyReservation)
				if err != nil {
					return nil, "", err
				}
			}
			if url != "" {
				payload[key] = map[string]string{"image_url": url}
			}
		}
	}
	body, err := json.Marshal(payload)
	return body, strings.TrimSuffix(chatgptCodexURL, "/responses") + endpoint, err
}
func (s *OpenAIGatewayService) buildOpenAIImagesNativeOAuthRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token, endpoint string) (*http.Request, error) {
	// Reuse all existing OAuth credential/header/isolation/profile checks. Only
	// the path changes on the same verified Codex origin, before the one send.
	req, err := s.buildUpstreamRequest(ctx, c, account, body, token, gjson.GetBytes(body, "stream").Bool(), "", false)
	if err != nil {
		return nil, err
	}
	req.URL.Path = strings.TrimSuffix(req.URL.Path, "/responses") + "/images/generations"
	if endpoint == openAIImagesEditsEndpoint {
		req.URL.Path = strings.TrimSuffix(req.URL.Path, "/generations") + "/edits"
	}
	// The common builder supplies identity and isolation headers; its Responses
	// client_metadata mutation is not a native Images API parameter.
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	trackOpenAIRequestBody(req, ctx)
	req.URL.RawPath = ""
	req.URL.RawQuery = ""
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAIMedia)), nil
}

// Parse native output without guessing a successful image from the requested n.
func nativeOpenAIImagesResults(body []byte) ([]openAIResponsesImageResult, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("invalid image response")
	}
	root := gjson.ParseBytes(body)
	results := make([]openAIResponsesImageResult, 0)
	for _, item := range root.Get("data").Array() {
		if item.Get("b64_json").String() == "" {
			return nil, fmt.Errorf("image stream requires inline image result")
		}
		meta := func(key string) string {
			if v := item.Get(key).String(); v != "" {
				return v
			}
			return root.Get(key).String()
		}
		results = append(results, openAIResponsesImageResult{Result: item.Get("b64_json").String(), RevisedPrompt: item.Get("revised_prompt").String(), OutputFormat: meta("output_format"), Size: meta("size"), Background: meta("background"), Quality: meta("quality"), Model: meta("model")})
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("image response contains no output")
	}
	return results, nil
}
func writeOpenAIImagesJSONAsCompleted(c *gin.Context, body []byte, parsed *OpenAIImagesRequest) error {
	results, err := nativeOpenAIImagesResults(body)
	if err != nil {
		return err
	}
	kind := "image_generation.completed"
	if parsed.IsEdits() {
		kind = "image_edit.completed"
	}
	usage := gjson.GetBytes(body, "usage")
	c.Header("Cache-Control", "no-cache")
	c.Header("Content-Type", "text/event-stream")
	c.Status(http.StatusOK)
	for i, img := range results {
		var raw []byte
		if i == len(results)-1 && usage.IsObject() {
			raw = []byte(usage.Raw)
		}
		if img.Model == "" {
			img.Model = parsed.Model
		}
		data := buildOpenAIImagesStreamCompletedPayload(kind, img, parsed.ResponseFormat, gjson.GetBytes(body, "created").Int(), raw)
		if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", kind, data); err != nil {
			return err
		}
		c.Writer.Flush()
	}
	return nil
}
func hasOpenAIImagesUsage(u OpenAIUsage) bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0
}

// Only explicit cache detail can separate image from text cache reads.
func nativeOpenAIImagesUsage(body []byte) (OpenAIUsage, bool) {
	value := gjson.GetBytes(body, "usage")
	u, ok := openAIUsageFromGJSON(value)
	if !ok {
		return u, false
	}
	if !value.Get("output_tokens_details.image_tokens").Exists() {
		u.ImageOutputTokens = max(u.OutputTokens, 0)
	}
	cached := value.Get("input_tokens_details.cached_tokens_details")
	bounded := func(v gjson.Result) int {
		if v.Type != gjson.Number || v.Float() < 0 || v.Float() > float64(math.MaxInt32) || v.Float() != math.Trunc(v.Float()) {
			return 0
		}
		return int(v.Int())
	}
	imageCached := bounded(cached.Get("image_tokens"))
	if !value.Get("input_tokens_details.cached_tokens").Exists() && cached.IsObject() {
		u.CacheReadInputTokens = min(imageCached, max(u.InputTokens, 0))
		u.CacheReadInputTokens += min(bounded(cached.Get("text_tokens")), max(u.InputTokens-u.CacheReadInputTokens, 0))
	}
	u.ImageCacheReadTokens = min(imageCached, max(u.ImageInputTokens, 0), max(u.CacheReadInputTokens, 0))
	return u, true
}

// Decode only a bounded header. No SVG rendering, entity expansion or URL fetch.
func openAIImageDimensions(data []byte) (int, int) {
	header := data
	if len(header) > 64<<10 {
		header = header[:64<<10]
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(header)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
		return cfg.Width, cfg.Height
	}
	d := xml.NewDecoder(bytes.NewReader(header))
	for {
		tok, err := d.Token()
		if err != nil {
			return 0, 0
		}
		if start, ok := tok.(xml.StartElement); ok {
			if start.Name.Local != "svg" {
				return 0, 0
			}
			var width, height int
			var viewBox string
			dimension := func(raw string) int {
				raw = strings.TrimSuffix(strings.TrimSpace(raw), "px")
				n, err := strconv.ParseFloat(raw, 64)
				if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n > math.MaxInt32 {
					return 0
				}
				return int(math.Ceil(n))
			}
			for _, attr := range start.Attr {
				switch attr.Name.Local {
				case "width":
					width = dimension(attr.Value)
				case "height":
					height = dimension(attr.Value)
				case "viewBox":
					viewBox = attr.Value
				}
			}
			if width > 0 && height > 0 {
				return width, height
			}
			values := strings.Fields(strings.ReplaceAll(viewBox, ",", " "))
			if len(values) == 4 {
				return dimension(values[2]), dimension(values[3])
			}
			return 0, 0
		}
	}
}
func openAIImageInlineSize(encoded string) string {
	if strings.HasPrefix(encoded, "data:") {
		i := strings.IndexByte(encoded, ',')
		if i < 0 {
			return ""
		}
		encoded = encoded[i+1:]
	}
	// A fixed prefix also bounds allocations for large base64 payloads.
	reader := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
	header, _ := io.ReadAll(io.LimitReader(reader, 64<<10))
	w, h := openAIImageDimensions(header)
	if w == 0 || h == 0 {
		return ""
	}
	return fmt.Sprintf("%dx%d", w, h)
}
func validateNativeImagesRoute(account *Account, model string) error {
	if !requiresCodexDirectImages(model) || account == nil {
		return nil
	}
	mode, _ := account.Extra[openai_compat.ExtraKeyResponsesMode].(string)
	if openai_compat.NormalizeResponsesSupportMode(mode) == openai_compat.ResponsesSupportModeForceResponses {
		return fmt.Errorf("GPT Image 2.6 requires the native Images endpoint; forced Responses routing is incompatible")
	}
	return nil
}

func backfillNativeImagesSizes(body []byte) []byte {
	for i, item := range gjson.GetBytes(body, "data").Array() {
		if size := openAIImageInlineSize(item.Get("b64_json").String()); size != "" {
			if updated, err := sjson.SetBytes(body, fmt.Sprintf("data.%d.size", i), size); err == nil {
				body = updated
			}
		}
	}
	return body
}

func reserveOpenAIImagesPayload(parsed *OpenAIImagesRequest) error {
	if parsed == nil || parsed.bodyReservation == nil {
		return nil
	}
	// JSON escaping may expand field strings sixfold; marshal's grow buffer and
	// returned copy can coexist. Upload base64 is registered separately above.
	bytes := int64(len(parsed.Body))*12 + 64<<10
	for _, upload := range parsed.Uploads {
		bytes += 3 * int64(base64.StdEncoding.EncodedLen(len(upload.Data)))
	}
	for _, upload := range []*OpenAIImagesUpload{parsed.MaskUpload, parsed.MarkupUpload} {
		if upload != nil {
			bytes += 3 * int64(base64.StdEncoding.EncodedLen(len(upload.Data)))
		}
	}
	return parsed.bodyReservation.Reserve(bytes)
}
