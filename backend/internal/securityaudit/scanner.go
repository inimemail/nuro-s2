package securityaudit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const maxGuardResponseBytes int64 = 256 << 10

type scanResult struct {
	Decision   string
	Risk       string
	Action     string
	Safety     string
	Categories []string
	Backend    string
	Version    string
	EndpointID string
	LatencyMS  int
}

type auditHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
	CloseIdleConnections()
}

var categoryAliases = map[string]string{
	"violent": "violent", "violence": "violent",
	"non violent illegal acts":      "non_violent_illegal_acts",
	"sexual content or sexual acts": "sexual_content_or_sexual_acts", "sexual": "sexual_content_or_sexual_acts",
	"pii": "pii", "personal identifying information": "pii", "personal identifiable information": "pii",
	"suicide self harm": "suicide_and_self_harm", "suicide and self harm": "suicide_and_self_harm",
	"unethical acts": "unethical_acts", "unethical": "unethical_acts",
	"politically sensitive topics": "politically_sensitive_topics", "political": "politically_sensitive_topics",
	"copyright violation": "copyright_violation", "copyright": "copyright_violation",
	"jailbreak": "jailbreak", "prompt injection": "jailbreak",
}

var knownCategories = func() map[string]struct{} {
	result := make(map[string]struct{}, len(defaultScanners))
	for _, category := range defaultScanners {
		result[category] = struct{}{}
	}
	return result
}()

var criticalCategories = map[string]struct{}{
	"jailbreak":             {},
	"pii":                   {},
	"suicide_and_self_harm": {},
}

func normalizeCategory(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("_", " ", "&", " and ", "/", " ", "-", " ").Replace(value)
	value = strings.Join(strings.Fields(value), " ")
	if canonical, ok := categoryAliases[value]; ok {
		return canonical
	}
	return strings.ReplaceAll(value, " ", "_")
}

func parseGuardResult(content string, enabled []string) (scanResult, error) {
	lines := make([]string, 0, 2)
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 {
		return scanResult{}, errors.New("guard response shape invalid")
	}
	var safety, rawCategories string
	seenSafety, seenCategories := false, false
	for _, line := range lines {
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "safety:"):
			if seenSafety {
				return scanResult{}, errors.New("guard response safety duplicated")
			}
			seenSafety = true
			safety = strings.TrimSpace(line[len("safety:"):])
		case strings.HasPrefix(lower, "categories:"):
			if seenCategories {
				return scanResult{}, errors.New("guard response categories duplicated")
			}
			seenCategories = true
			rawCategories = strings.TrimSpace(line[len("categories:"):])
		default:
			return scanResult{}, errors.New("guard response field invalid")
		}
	}
	if !seenSafety || !seenCategories {
		return scanResult{}, errors.New("guard response fields missing")
	}
	switch strings.ToLower(safety) {
	case "safe":
		safety = "Safe"
	case "controversial":
		safety = "Controversial"
	case "unsafe":
		safety = "Unsafe"
	default:
		return scanResult{}, errors.New("guard safety value invalid")
	}
	enabledSet := make(map[string]struct{}, len(enabled))
	for _, scanner := range enabled {
		enabledSet[normalizeCategory(scanner)] = struct{}{}
	}
	categories := make([]string, 0)
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(rawCategories, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.EqualFold(raw, "none") || strings.EqualFold(raw, "n/a") {
			continue
		}
		category := normalizeCategory(raw)
		if _, known := knownCategories[category]; !known {
			category = unknownCategoryID(category)
		} else {
			_, critical := criticalCategories[category]
			if _, allowed := enabledSet[category]; !allowed && !critical {
				continue
			}
		}
		if _, exists := seen[category]; exists {
			continue
		}
		seen[category] = struct{}{}
		categories = append(categories, category)
	}
	sort.Strings(categories)
	result := scanResult{Decision: "pass", Risk: "low", Action: "Allow", Safety: safety, Categories: categories, Backend: "qwen3guard-openai"}
	hasCriticalCategory := false
	for _, category := range categories {
		if _, ok := criticalCategories[category]; ok {
			hasCriticalCategory = true
			break
		}
	}
	switch safety {
	case "Safe":
		switch {
		case hasCriticalCategory:
			result.Decision, result.Risk, result.Action = "critical", "critical", "Block"
		case len(categories) > 0:
			result.Decision, result.Risk, result.Action = "flag", "medium", "Warn"
		}
	case "Controversial":
		result.Decision, result.Risk, result.Action = "flag", "medium", "Warn"
		if hasCriticalCategory {
			result.Decision, result.Risk, result.Action = "critical", "critical", "Block"
		}
	case "Unsafe":
		result.Decision, result.Risk, result.Action = "critical", "critical", "Block"
	}
	return result, nil
}

func unknownCategoryID(value string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(value))))
	return "unknown:" + hex.EncodeToString(digest[:8])
}

func (s *Service) scan(ctx context.Context, cfg Config, text string) (scanResult, error) {
	var lastErr error
	for _, endpoint := range cfg.Endpoints {
		if !endpoint.Enabled {
			continue
		}
		result, err := s.scanEndpoint(ctx, endpoint, cfg.Scanners, text)
		if err == nil {
			return result, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no enabled audit endpoint")
	}
	return scanResult{}, lastErr
}

func (s *Service) scanEndpoint(ctx context.Context, endpoint EndpointConfig, scanners []string, text string) (scanResult, error) {
	client, endpointURL, err := secureEndpointClient(endpoint)
	if err != nil {
		return scanResult{}, err
	}
	return s.scanEndpointWithClient(ctx, endpoint, scanners, text, client, endpointURL)
}

func (s *Service) scanEndpointWithClient(ctx context.Context, endpoint EndpointConfig, scanners []string, text string, client auditHTTPClient, endpointURL string) (scanResult, error) {
	defer client.CloseIdleConnections()
	token := ""
	if endpoint.TokenCiphertext != "" {
		if s == nil || s.encryptor == nil {
			return scanResult{}, errors.New("endpoint token decryptor unavailable")
		}
		decryptedToken, err := s.encryptor.Decrypt(endpoint.TokenCiphertext)
		if err != nil {
			return scanResult{}, errors.New("endpoint token unavailable")
		}
		token = decryptedToken
	}
	payload := map[string]any{
		"model":       endpoint.Model,
		"messages":    []map[string]string{{"role": "user", "content": text}},
		"temperature": 0,
		"max_tokens":  64,
		"seed":        42,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return scanResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(endpoint.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpointURL, bytes.NewReader(body))
	if err != nil {
		return scanResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return scanResult{}, errors.New("guard endpoint unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return scanResult{}, errors.New("guard endpoint rejected request")
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxGuardResponseBytes+1))
	if err != nil || int64(len(responseBody)) > maxGuardResponseBytes {
		return scanResult{}, errors.New("guard endpoint response invalid")
	}
	content, err := extractOpenAIContent(responseBody)
	if err != nil {
		return scanResult{}, err
	}
	result, err := parseGuardResult(content, scanners)
	if err != nil {
		return scanResult{}, err
	}
	result.EndpointID = endpoint.ID
	result.Version = endpoint.Model
	result.LatencyMS = int(time.Since(started).Milliseconds())
	return result, nil
}

func extractOpenAIContent(body []byte) (string, error) {
	var response struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &response); err != nil || len(response.Choices) == 0 {
		return "", errors.New("guard response envelope invalid")
	}
	switch content := response.Choices[0].Message.Content.(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			return "", errors.New("guard response content empty")
		}
		return content, nil
	case []any:
		parts := make([]string, 0, len(content))
		for _, item := range content {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := object["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n"), nil
		}
	}
	return "", errors.New("guard response content invalid")
}

func normalizeEndpointURL(endpoint EndpointConfig) (string, error) {
	raw := strings.TrimSpace(endpoint.BaseURL)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", infraerrors.BadRequest("PROMPT_AUDIT_ENDPOINT_URL_INVALID", "audit endpoint URL is invalid")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", infraerrors.BadRequest("PROMPT_AUDIT_ENDPOINT_SCHEME_INVALID", "audit endpoint must use HTTP(S)")
	}
	if parsed.Scheme == "http" && !endpoint.AllowPrivate {
		return "", infraerrors.BadRequest("PROMPT_AUDIT_ENDPOINT_HTTPS_REQUIRED", "public audit endpoints must use HTTPS")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", infraerrors.BadRequest("PROMPT_AUDIT_ENDPOINT_URL_UNSAFE", "audit endpoint URL cannot contain credentials, query or fragment")
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if strings.EqualFold(path, "/v1") {
		path = ""
	}
	parsed.Path, parsed.RawPath = path, ""
	return strings.TrimRight(parsed.String(), "/") + "/v1/chat/completions", nil
}

func endpointCredentialBindingEqual(configured, candidate EndpointConfig) bool {
	configuredURL, err := normalizeEndpointURL(configured)
	if err != nil {
		return false
	}
	candidateURL, err := normalizeEndpointURL(candidate)
	if err != nil {
		return false
	}
	configuredParsed, err := url.Parse(configuredURL)
	if err != nil {
		return false
	}
	candidateParsed, err := url.Parse(candidateURL)
	if err != nil {
		return false
	}
	return configuredParsed.Scheme == candidateParsed.Scheme &&
		strings.EqualFold(configuredParsed.Hostname(), candidateParsed.Hostname()) &&
		configuredParsed.Port() == candidateParsed.Port() &&
		configuredParsed.EscapedPath() == candidateParsed.EscapedPath()
}

func secureEndpointClient(endpoint EndpointConfig) (*http.Client, string, error) {
	endpointURL, err := normalizeEndpointURL(endpoint)
	if err != nil {
		return nil, "", err
	}
	parsed, _ := url.Parse(endpointURL)
	allowed, err := parseAllowedCIDRs(endpoint.AllowedCIDRs)
	if err != nil {
		return nil, "", err
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil, ForceAttemptHTTP2: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 4,
		IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return nil, errors.New("audit endpoint address invalid")
		}
		addresses, lookupErr := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if lookupErr != nil || len(addresses) == 0 {
			return nil, errors.New("audit endpoint resolution failed")
		}
		for _, addressIP := range addresses {
			ip := addressIP.Unmap()
			if !auditDestinationAllowed(endpoint, parsed.Scheme, ip, allowed) {
				continue
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		return nil, errors.New("audit endpoint destination is not allowed")
	}
	timeout := time.Duration(endpoint.TimeoutMS) * time.Millisecond
	return &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}, parsed.String(), nil
}

func auditDestinationAllowed(endpoint EndpointConfig, scheme string, ip netip.Addr, allowed []netip.Prefix) bool {
	ip = ip.Unmap()
	if forbiddenAuditIP(ip) {
		return false
	}
	restricted := auditIPRequiresAllowlist(ip)
	if !ip.IsGlobalUnicast() && !restricted {
		return false
	}
	if restricted && (!endpoint.AllowPrivate || !ipAllowed(ip, allowed)) {
		return false
	}
	// Cleartext is only permitted for an explicitly allowlisted private or
	// special destination. Setting AllowPrivate must never downgrade a public
	// endpoint from HTTPS to HTTP.
	if strings.EqualFold(strings.TrimSpace(scheme), "http") && !restricted {
		return false
	}
	return true
}

func forbiddenAuditIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip == netip.MustParseAddr("169.254.169.254") || ip == netip.MustParseAddr("100.100.100.200") {
		return true
	}
	return false
}

var auditRestrictedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func auditIPRequiresAllowlist(ip netip.Addr) bool {
	if ip.IsPrivate() || ip.IsLoopback() {
		return true
	}
	for _, prefix := range auditRestrictedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func validateAllowedCIDRs(values []string) error {
	_, err := parseAllowedCIDRs(values)
	return err
}

func parseAllowedCIDRs(values []string) ([]netip.Prefix, error) {
	result := make([]netip.Prefix, 0, len(values))
	for _, raw := range values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, infraerrors.BadRequest("PROMPT_AUDIT_CIDR_INVALID", "audit endpoint CIDR allowlist contains an invalid entry")
		}
		result = append(result, prefix.Masked())
	}
	return result, nil
}

func ipAllowed(ip netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}
