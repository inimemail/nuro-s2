package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const exaSearchEndpoint = "https://api.exa.ai/search"

type ExaProvider struct {
	apiKey     string
	httpClient *http.Client
}

func NewExaProvider(apiKey string, httpClient *http.Client) *ExaProvider {
	return &ExaProvider{apiKey: apiKey, httpClient: searchProviderClient(httpClient)}
}

func (e *ExaProvider) Name() string { return ProviderTypeExa }

func (e *ExaProvider) Search(ctx context.Context, search SearchRequest) (*SearchResponse, error) {
	count := search.MaxResults
	if count <= 0 {
		count = defaultMaxResults
	}
	count = min(count, 20)
	body, err := json.Marshal(struct {
		Query      string          `json:"query"`
		NumResults int             `json:"numResults"`
		Contents   map[string]bool `json:"contents"`
	}{search.Query, count, map[string]bool{"highlights": true}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, exaSearchEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", e.apiKey)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isProxyError(err) {
			return nil, ErrProxyUnavailable
		}
		return nil, errors.New("exa: search request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exa: unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isProxyError(err) {
			return nil, ErrProxyUnavailable
		}
		return nil, errors.New("exa: search response could not be read")
	}
	if len(data) > maxResponseSize {
		return nil, errors.New("exa: search response could not be read")
	}
	var decoded struct {
		Results []struct {
			URL           string   `json:"url"`
			Title         string   `json:"title"`
			Highlights    []string `json:"highlights"`
			PublishedDate string   `json:"publishedDate"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, errors.New("exa: invalid search response")
	}
	results := make([]SearchResult, 0, min(count, len(decoded.Results)))
	for _, result := range decoded.Results {
		address, err := url.Parse(result.URL)
		if err != nil || address.Hostname() == "" || address.User != nil ||
			(address.Scheme != "http" && address.Scheme != "https") {
			continue
		}
		results = append(results, SearchResult{
			URL: address.String(), Title: result.Title,
			Snippet: strings.Join(result.Highlights, "\n"), PageAge: result.PublishedDate,
		})
		if len(results) == count {
			break
		}
	}
	return &SearchResponse{Results: results, Query: search.Query}, nil
}
