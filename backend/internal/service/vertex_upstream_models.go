package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func (s *AccountTestService) fetchVertexGeminiUpstreamModels(ctx context.Context, account *Account) ([]string, error) {
	if account.IsVertexAPIKey() {
		return nil, newUpstreamModelSyncUnsupportedError("Vertex API key accounts do not support model listing", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.geminiTokenProvider == nil {
		return nil, newUpstreamModelSyncConfigError("Gemini token provider is not configured", nil)
	}
	location := account.VertexLocation("")
	if !vertexLocationPattern.MatchString(location) {
		return nil, newUpstreamModelSyncConfigError("Invalid Vertex location", nil)
	}
	token, err := s.geminiTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to get Vertex token", err)
	}
	host := location + "-aiplatform.googleapis.com"
	if location == "global" {
		host = "aiplatform.googleapis.com"
	}
	return listVertexGeminiPublisherModels(func(pageToken string) ([]byte, error) {
		params := url.Values{"pageSize": {"300"}}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/v1beta1/publishers/google/models?"+params.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		resp, err := s.doUpstreamModelsRequest(req, upstreamModelsProxyURL(account), account)
		if err != nil {
			return nil, newUpstreamModelSyncUpstreamError("Failed to request Vertex models", err)
		}
		defer func() { _ = resp.Body.Close() }()
		limit := resolveModelsListReadLimit(s.cfg)
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil || int64(len(body)) > limit {
			return nil, newUpstreamModelSyncUpstreamError("Invalid Vertex model list response", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if pageToken == "" && (resp.StatusCode == 404 || resp.StatusCode == 405) {
				// Only a missing first-page endpoint permits the configured list fallback.
				return nil, newUpstreamModelSyncUnsupportedError("Vertex model listing endpoint is unavailable; use configured models", nil)
			}
			return nil, newUpstreamModelSyncUpstreamError(fmt.Sprintf("Vertex model list failed with HTTP %d", resp.StatusCode), nil)
		}
		return body, nil
	})
}

func listVertexGeminiPublisherModels(fetch func(string) ([]byte, error)) ([]string, error) {
	seenTokens := make(map[string]bool)
	models := make(map[string]bool)
	pageToken := ""
	for page := 0; page < 20; page++ {
		if seenTokens[pageToken] {
			return nil, newUpstreamModelSyncUpstreamError("Vertex model pagination repeated a page", nil)
		}
		seenTokens[pageToken] = true
		body, err := fetch(pageToken)
		if err != nil {
			return nil, err
		}
		var response struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"publisherModels"`
			Next string `json:"nextPageToken"`
		}
		if json.Unmarshal(body, &response) != nil || len(response.Models) > 300 || len(response.Next) > 4096 {
			return nil, newUpstreamModelSyncUpstreamError("Invalid Vertex model list", nil)
		}
		for _, model := range response.Models {
			id, ok := strings.CutPrefix(model.Name, "publishers/google/models/")
			if ok && strings.HasPrefix(id, "gemini-") && !strings.ContainsAny(id, "/?#") && len(id) <= 256 {
				models[id] = true
			}
		}
		pageToken = response.Next
		if pageToken == "" {
			if len(models) == 0 {
				return nil, newUpstreamModelSyncUpstreamError("Vertex returned no supported Gemini models", nil)
			}
			result := make([]string, 0, len(models))
			for model := range models {
				result = append(result, model)
			}
			sort.Strings(result)
			return result, nil
		}
	}
	return nil, newUpstreamModelSyncUpstreamError("Vertex model pagination exceeded 20 pages", nil)
}
