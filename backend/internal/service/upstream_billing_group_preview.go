package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const upstreamBillingGroupPreviewConcurrency = 4

var (
	errUpstreamBillingUnsupported = errors.New("unsupported")
	errUpstreamGroupPreviewBusy   = infraerrors.New(http.StatusTooManyRequests, "UPSTREAM_GROUP_PREVIEW_BUSY", "Too many group previews are in progress")
	errUpstreamGroupPreviewFailed = infraerrors.New(http.StatusBadGateway, "UPSTREAM_GROUP_PREVIEW_FAILED", "Could not read group information from the upstream")
)

// PreviewGroup uses only the saved account identity. Preview traffic has its own
// admission limit and never updates the periodic probe snapshot.
func (s *UpstreamBillingProbeService) PreviewGroup(ctx context.Context, accountID int64) (BillingGroupInfo, error) {
	if s == nil || s.accountRepo == nil {
		return BillingGroupInfo{}, ErrUpstreamBillingProbeUnavailable
	}
	if err := ctx.Err(); err != nil {
		return BillingGroupInfo{}, errUpstreamGroupPreviewFailed
	}
	select {
	case s.groupPreviewSlots <- struct{}{}:
		defer func() { <-s.groupPreviewSlots }()
	default:
		return BillingGroupInfo{}, errUpstreamGroupPreviewBusy
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamBillingProbeRequestTimeout)
	defer cancel()
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if ctx.Err() != nil {
		return BillingGroupInfo{}, errUpstreamGroupPreviewFailed
	}
	if errors.Is(err, ErrAccountNotFound) {
		return BillingGroupInfo{}, ErrAccountNotFound
	}
	if err != nil {
		return BillingGroupInfo{}, errUpstreamGroupPreviewFailed
	}
	if !isUpstreamBillingProbeAccount(account) || account.IsCredentialShadow() || account.IsImagePoolMode() {
		return BillingGroupInfo{}, ErrUpstreamBillingProbeAccountInvalid
	}
	baseURL := strings.TrimSpace(account.GetCredential("base_url"))
	if baseURL == "" {
		return BillingGroupInfo{}, nil
	}
	target, err := url.Parse(baseURL)
	if err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		return BillingGroupInfo{}, errUpstreamGroupPreviewFailed
	}
	if isOfficialUpstreamBillingPreviewTarget(baseURL, target.Hostname()) {
		return BillingGroupInfo{}, nil
	}
	reply, err := s.fetchBilling(ctx, account, true)
	if ctx.Err() != nil {
		return BillingGroupInfo{}, errUpstreamGroupPreviewFailed
	}
	if errors.Is(err, errUpstreamBillingUnsupported) {
		return BillingGroupInfo{}, nil
	}
	if err != nil {
		return BillingGroupInfo{}, errUpstreamGroupPreviewFailed
	}
	info, err := parseUpstreamBillingGroupInfo(reply.body)
	if err != nil {
		return BillingGroupInfo{}, err
	}
	info.Name = filterUpstreamBillingGroupText(info.Name, account, target)
	info.Description = filterUpstreamBillingGroupText(info.Description, account, target)
	return info, nil
}

func isOfficialUpstreamBillingPreviewTarget(baseURL, hostname string) bool {
	host := strings.TrimSuffix(strings.ToLower(hostname), ".")
	for _, domain := range []string{"moonshot.cn", "kimi.com", "z.ai"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return upstreamBillingProbeTargetIsOfficialAPI(baseURL)
}

func filterUpstreamBillingGroupText(text string, account *Account, target *url.URL) string {
	if text == "" {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
	if strings.Contains(strings.ToLower(text), host) || redactOpsPlainTextSecrets(text) != text || containsSavedAccountSecret(account, text) {
		return ""
	}
	return text
}

func parseUpstreamBillingGroupInfo(body []byte) (BillingGroupInfo, error) {
	var payload struct {
		Object        string          `json:"object"`
		SchemaVersion int             `json:"schema_version"`
		BillingScope  string          `json:"billing_scope"`
		Name          json.RawMessage `json:"group_name"`
		Description   json.RawMessage `json:"group_description"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Object != "sub2api.key_billing" || payload.SchemaVersion != 1 || payload.BillingScope != "token" {
		return BillingGroupInfo{}, errUpstreamGroupPreviewFailed
	}
	return BillingGroupInfo{
		Name:        upstreamBillingGroupText(payload.Name, 100, false),
		Description: upstreamBillingGroupText(payload.Description, 4096, true),
	}, nil
}

func upstreamBillingGroupText(raw json.RawMessage, maxBytes int, multiline bool) string {
	var text string
	if !utf8.Valid(raw) || json.Unmarshal(raw, &text) != nil {
		return ""
	}
	text = strings.TrimSpace(text)
	if len(text) > maxBytes {
		return ""
	}
	for _, r := range text {
		if unicode.Is(unicode.Bidi_Control, r) || (unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\r' || r == '\t'))) {
			return ""
		}
		if !multiline && (r == '\u2028' || r == '\u2029') {
			return ""
		}
	}
	return text
}
