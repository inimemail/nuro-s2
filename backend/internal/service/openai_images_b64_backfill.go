package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AccountExtraImagesURLToB64JSON enables URL -> b64_json backfill for the
// non-streaming Images response. It is intentionally stored in account Extra,
// keeping this feature independent from the protected billing/cache fields.
const AccountExtraImagesURLToB64JSON = "images_url_to_b64_json"

const openAIImageURLBackfillMaxBytes = 20 << 20

func ImagesURLToB64JSONEnabled(account *Account) bool {
	return account != nil && account.getExtraBool(AccountExtraImagesURLToB64JSON)
}

func (s *OpenAIGatewayService) backfillOpenAIImagesB64JSON(ctx context.Context, account *Account, parsed *OpenAIImagesRequest, body []byte) []byte {
	if !ImagesURLToB64JSONEnabled(account) || (parsed != nil && strings.EqualFold(parsed.ResponseFormat, "url")) || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	items := gjson.GetBytes(body, "data")
	if !items.IsArray() {
		return body
	}
	for index, item := range items.Array() {
		if !item.IsObject() || strings.TrimSpace(item.Get("b64_json").String()) != "" {
			continue
		}
		rawURL := strings.TrimSpace(item.Get("url").String())
		if rawURL == "" {
			continue
		}
		encoded, err := s.fetchOpenAIImageURLBase64(ctx, account, rawURL)
		if err != nil {
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Images b64_json backfill skipped account_id=%d index=%d err=%s", account.ID, index, sanitizeUpstreamErrorMessage(err.Error()))
			continue
		}
		if updated, err := sjson.SetBytes(body, fmt.Sprintf("data.%d.b64_json", index), encoded); err == nil {
			body = updated
		}
	}
	return body
}

func (s *OpenAIGatewayService) fetchOpenAIImageURLBase64(ctx context.Context, account *Account, rawURL string) (string, error) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawURL)), "data:") {
		if encoded := normalizeOpenAIImageBase64(rawURL); encoded != "" {
			return encoded, nil
		}
		return "", errors.New("data URL payload is not valid base64")
	}
	if s == nil || s.cfg == nil || s.httpUpstream == nil || account == nil {
		return "", errors.New("image URL downloader is not configured")
	}
	downloadURL, err := s.validateUpstreamBaseURL(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid image URL: %w", err)
	}
	parsed, err := url.Parse(downloadURL)
	if err != nil || parsed.Hostname() == "" || isPrivateImageHost(parsed.Hostname()) {
		return "", errors.New("image URL host is not allowed")
	}
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
		return "", errors.New("image URL host is not public")
	}
	// The literal-host checks above do not protect against a public hostname
	// resolving to a private address. Reuse the shared DNS validation unless
	// the deployment explicitly opted into private upstream hosts.
	allowPrivate := s.cfg != nil && s.cfg.Security.URLAllowlist.AllowPrivateHosts
	if !allowPrivate {
		if err := urlvalidator.ValidateResolvedIP(parsed.Hostname()); err != nil {
			return "", fmt.Errorf("image URL host is not public: %w", err)
		}
	}
	requestCtx, cancel := context.WithTimeout(WithHTTPUpstreamRedirectsDisabled(ctx), openAIImageURLBackfillTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return "", fmt.Errorf("download image: %w", err)
	}
	if resp == nil || resp.Body == nil {
		return "", errors.New("download image: empty response")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download image: unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, openAIImageURLBackfillMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > openAIImageURLBackfillMaxBytes {
		return "", errors.New("download image: invalid size")
	}
	contentType := http.DetectContentType(data)
	switch contentType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return "", fmt.Errorf("download image: unsupported content type %s", contentType)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func isPrivateImageHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified())
}

const openAIImageURLBackfillTimeout = 60 * time.Second
