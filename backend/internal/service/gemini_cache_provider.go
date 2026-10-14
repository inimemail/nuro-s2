package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
)

var vertexCacheProjectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var geminiCacheUpstreamNamePattern = regexp.MustCompile(`^(cachedContents/[A-Za-z0-9_-]+|projects/[A-Za-z0-9._-]+/locations/[a-z0-9-]+/cachedContents/[A-Za-z0-9_-]+)$`)

type GeminiCacheProviderResult struct {
	Name, Model, DisplayName string
	Tokens                   int64
	ExpiresAt                time.Time
}
type GeminiCacheProviderError struct {
	Status            int
	Uncertain         bool
	ConfirmedNotFound bool
}

func (e *GeminiCacheProviderError) Error() string { return "cache provider operation failed" }

func IsExplicitCacheAccount(account *Account) bool {
	if account == nil || account.Platform != PlatformGemini || account.IsPoolMode() || account.IsVertexAPIKey() {
		return false
	}
	if account.Type == AccountTypeServiceAccount {
		return true
	}
	if account.Type != AccountTypeAPIKey {
		return false
	}
	if isOfficialGeminiAPIBaseURL(account.GetCredential("base_url")) {
		return true
	}
	enabled, _ := account.Credentials["gemini_explicit_cache_enabled"].(bool)
	maxTTL := GeminiCacheAccountMaxTTL(account)
	return enabled && maxTTL >= time.Minute && maxTTL <= 24*time.Hour
}

func buildGeminiCacheVertexEndpoint(account *Account, path string, body []byte) (string, error) {
	project, location := account.VertexProjectID(), account.VertexLocation("")
	if strings.HasPrefix(path, "/projects/") {
		segments := strings.Split(strings.SplitN(strings.TrimPrefix(path, "/"), "?", 2)[0], "/")
		if len(segments) != 6 || segments[1] != project || segments[2] != "locations" || segments[4] != "cachedContents" {
			return "", errors.New("invalid Vertex cache resource binding")
		}
		location = segments[3]
	} else if len(body) > 0 {
		var request struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &request) != nil {
			return "", errors.New("invalid Vertex cache request")
		}
		if strings.HasPrefix(request.Model, "projects/") {
			segments := strings.Split(request.Model, "/")
			if len(segments) != 8 || segments[1] != project || segments[2] != "locations" || segments[4] != "publishers" || segments[5] != "google" || segments[6] != "models" {
				return "", errors.New("invalid Vertex cache model binding")
			}
			location = segments[3]
		} else if request.Model != "" {
			location = account.VertexLocation(lastSegment(request.Model))
		}
	}
	if !vertexCacheProjectPattern.MatchString(project) || !vertexLocationPattern.MatchString(location) {
		return "", errors.New("invalid Vertex cache endpoint")
	}
	host := location + "-aiplatform.googleapis.com"
	if location == "global" {
		host = "aiplatform.googleapis.com"
	}
	endpoint := "https://" + host + "/v1/projects/" + url.PathEscape(project) + "/locations/" + location + "/" + strings.TrimPrefix(path, "/")
	if strings.HasPrefix(path, "/projects/") {
		endpoint = "https://" + host + "/v1" + path
	}
	return endpoint, nil
}

func (s *GeminiMessagesCompatService) cacheProviderRequest(ctx context.Context, account *Account, method, path string, body []byte) ([]byte, int, error) {
	if !IsExplicitCacheAccount(account) {
		return nil, 0, errors.New("account does not support explicit caching")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var endpoint string
	var err error
	if account.IsVertexServiceAccount() {
		endpoint, err = buildGeminiCacheVertexEndpoint(account, path, body)
		if err != nil {
			return nil, 0, err
		}
	} else {
		base := account.GetGeminiBaseURL(geminicli.AIStudioBaseURL)
		base, err = s.validateUpstreamBaseURL(base)
		if err != nil {
			return nil, 0, err
		}
		base = strings.TrimRight(base, "/")
		if strings.HasSuffix(base, "/v1beta") {
			endpoint = base + path
		} else {
			endpoint = base + "/v1beta" + path
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if account.IsVertexServiceAccount() {
		if err = setVertexGeminiAuth(ctx, req, account, s.tokenProvider); err != nil {
			return nil, 0, err
		}
	} else {
		key := strings.TrimSpace(account.GetCredential("api_key"))
		if key == "" || strings.ContainsAny(key, "\r\n") {
			return nil, 0, errors.New("invalid Gemini cache credential")
		}
		req.Header.Set("x-goog-api-key", key)
	}
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.DoWithTLS(req, proxy, account.ID, account.Concurrency, s.resolveTLSProfile(account))
	if err != nil {
		return nil, 0, &GeminiCacheProviderError{Uncertain: method != http.MethodGet}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return nil, resp.StatusCode, &GeminiCacheProviderError{Status: resp.StatusCode, Uncertain: method != http.MethodGet}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, &GeminiCacheProviderError{Status: resp.StatusCode, Uncertain: method != http.MethodGet && !cacheProviderRejection(resp.StatusCode, data), ConfirmedNotFound: cacheProviderConfirmedNotFound(resp.StatusCode, data)}
	}
	return data, resp.StatusCode, nil
}

func parseGeminiCacheProviderResult(body []byte) (*GeminiCacheProviderResult, error) {
	fields, err := parseGeminiCacheJSON(body)
	if err != nil {
		return nil, err
	}
	var result struct {
		Name        string    `json:"name"`
		Model       string    `json:"model"`
		DisplayName string    `json:"displayName"`
		Expiry      time.Time `json:"expireTime"`
		Usage       struct {
			Tokens int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if len(fields) == 0 || json.Unmarshal(body, &result) != nil || !geminiCacheUpstreamNamePattern.MatchString(result.Name) || result.Expiry.IsZero() || result.Usage.Tokens <= 0 || len(result.Name) > 512 || len(result.Model) > 512 {
		return nil, errors.New("invalid cache provider result")
	}
	return &GeminiCacheProviderResult{Name: result.Name, Model: result.Model, DisplayName: result.DisplayName, Tokens: result.Usage.Tokens, ExpiresAt: result.Expiry.UTC()}, nil
}

func (s *GeminiMessagesCompatService) CountCacheTokens(ctx context.Context, account *Account, model string, fields map[string]json.RawMessage) (int64, error) {
	// Restrict quotation to content whose token count the provider can certify.
	// File/media payloads require a verified cost ceiling and are not guessed.
	if err := validateGeminiCachePayload(fields); err != nil {
		return 0, err
	}
	request := map[string]json.RawMessage{}
	for _, field := range []string{"contents", "systemInstruction", "tools", "toolConfig"} {
		if raw, ok := fields[field]; ok {
			request[field] = raw
		}
	}
	modelJSON, _ := json.Marshal("models/" + model)
	request["model"] = modelJSON
	generateBody, _ := json.Marshal(request)
	body, _ := json.Marshal(map[string]json.RawMessage{"generateContentRequest": generateBody})
	path := "/models/" + url.PathEscape(model) + ":countTokens"
	if account.IsVertexServiceAccount() {
		path = "/publishers/google/models/" + url.PathEscape(model) + ":countTokens"
		body = generateBody
	}
	data, _, err := s.cacheProviderRequest(ctx, account, http.MethodPost, path, body)
	if err != nil {
		return 0, err
	}
	fieldsResult, err := parseGeminiCacheJSON(data)
	if err != nil {
		return 0, err
	}
	var count int64
	if json.Unmarshal(fieldsResult["totalTokens"], &count) != nil || count <= 0 || count > 1_000_000_000 {
		return 0, errors.New("cache token quotation unavailable")
	}
	return count, nil
}
func (s *GeminiMessagesCompatService) CreateCacheContent(ctx context.Context, account *Account, model, operationID string, request *GeminiCacheCreateRequest) (*GeminiCacheProviderResult, error) {
	fields := make(map[string]json.RawMessage, len(request.Body))
	for k, v := range request.Body {
		fields[k] = v
	}
	delete(fields, "ttl")
	nameJSON, _ := json.Marshal("nuro-op-" + operationID)
	fields["displayName"] = nameJSON
	modelJSON, _ := json.Marshal("models/" + model)
	fields["model"] = modelJSON
	expiryJSON, _ := json.Marshal(request.ExpiresAt.UTC().Format(time.RFC3339Nano))
	fields["expireTime"] = expiryJSON
	if account.IsVertexServiceAccount() {
		modelJSON, _ = json.Marshal("projects/" + account.VertexProjectID() + "/locations/" + account.VertexLocation(model) + "/publishers/google/models/" + model)
		fields["model"] = modelJSON
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	data, _, err := s.cacheProviderRequest(ctx, account, http.MethodPost, "/cachedContents", body)
	if err != nil {
		return nil, err
	}
	result, err := parseGeminiCacheProviderResult(data)
	if err != nil {
		return nil, &GeminiCacheProviderError{Uncertain: true}
	}
	return result, nil
}
func (s *GeminiMessagesCompatService) GetCacheContent(ctx context.Context, account *Account, name string) (*GeminiCacheProviderResult, error) {
	if !geminiCacheUpstreamNamePattern.MatchString(name) {
		return nil, errors.New("invalid upstream cache name")
	}
	data, _, err := s.cacheProviderRequest(ctx, account, http.MethodGet, "/"+name, nil)
	if err != nil {
		return nil, err
	}
	return parseGeminiCacheProviderResult(data)
}
func (s *GeminiMessagesCompatService) PatchCacheContent(ctx context.Context, account *Account, name string, target time.Time) (*GeminiCacheProviderResult, error) {
	if !geminiCacheUpstreamNamePattern.MatchString(name) {
		return nil, errors.New("invalid upstream cache name")
	}
	body, _ := json.Marshal(map[string]string{"expireTime": target.UTC().Format(time.RFC3339Nano)})
	data, _, err := s.cacheProviderRequest(ctx, account, http.MethodPatch, "/"+name+"?updateMask=expire_time", body)
	if err != nil {
		return nil, err
	}
	result, err := parseGeminiCacheProviderResult(data)
	if err != nil {
		return nil, &GeminiCacheProviderError{Uncertain: true}
	}
	return result, nil
}
func (s *GeminiMessagesCompatService) DeleteCacheContent(ctx context.Context, account *Account, name string) error {
	if !geminiCacheUpstreamNamePattern.MatchString(name) {
		return errors.New("invalid upstream cache name")
	}
	_, _, err := s.cacheProviderRequest(ctx, account, http.MethodDelete, "/"+name, nil)
	var providerErr *GeminiCacheProviderError
	if errors.As(err, &providerErr) && providerErr.ConfirmedNotFound {
		return nil
	}
	return err
}
func (s *GeminiMessagesCompatService) FindCacheOperation(ctx context.Context, account *Account, operationID string) (*GeminiCacheProviderResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	page := ""
	seen := map[string]bool{}
	var found *GeminiCacheProviderResult
	for i := 0; i < 20; i++ {
		if seen[page] {
			return nil, errors.New("cache pagination loop")
		}
		seen[page] = true
		query := url.Values{"pageSize": {"100"}}
		if page != "" {
			query.Set("pageToken", page)
		}
		body, _, err := s.cacheProviderRequest(ctx, account, http.MethodGet, "/cachedContents?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var list struct {
			Items []json.RawMessage `json:"cachedContents"`
			Next  string            `json:"nextPageToken"`
		}
		if _, err := parseGeminiCacheJSON(body); err != nil {
			return nil, err
		}
		if json.Unmarshal(body, &list) != nil || len(list.Items) > 100 || len(list.Next) > 4096 {
			return nil, errors.New("invalid cache list")
		}
		for _, item := range list.Items {
			result, err := parseGeminiCacheProviderResult(item)
			if err != nil {
				return nil, err
			}
			if result.DisplayName == "nuro-op-"+operationID {
				if found != nil {
					return nil, ErrGeminiCacheConflict
				}
				found = result
			}
		}
		page = list.Next
		if page == "" {
			if found == nil {
				return nil, ErrGeminiCacheNotFound
			}
			return found, nil
		}
	}
	return nil, errors.New("cache operation search exceeded pagination bound")
}

func GeminiCacheAccountMaxTTL(account *Account) time.Duration {
	if account == nil {
		return 0
	}
	if account.IsVertexServiceAccount() || isOfficialGeminiAPIBaseURL(account.GetCredential("base_url")) {
		return 24 * time.Hour
	}
	raw := account.Credentials["gemini_explicit_cache_max_ttl_seconds"]
	var v float64
	switch n := raw.(type) {
	case float64:
		v = n
	case int:
		v = float64(n)
	case int64:
		v = float64(n)
	case json.Number:
		v, _ = strconv.ParseFloat(string(n), 64)
	default:
		return 0
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v < 60 || v > 86400 {
		return 0
	}
	return time.Duration(v * float64(time.Second))
}

func cacheProviderRejection(status int, body []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	if _, err := parseGeminiCacheJSON(body); err != nil {
		return false
	}
	var envelope struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Error.Code != status || envelope.Error.Message == "" {
		return false
	}
	switch envelope.Error.Status {
	case "INVALID_ARGUMENT", "UNAUTHENTICATED", "PERMISSION_DENIED", "NOT_FOUND", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "ALREADY_EXISTS", "OUT_OF_RANGE":
		return true
	}
	return false
}

func cacheProviderConfirmedNotFound(status int, body []byte) bool {
	if status != http.StatusNotFound || !cacheProviderRejection(status, body) {
		return false
	}
	var envelope struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &envelope) == nil && envelope.Error.Status == "NOT_FOUND"
}
