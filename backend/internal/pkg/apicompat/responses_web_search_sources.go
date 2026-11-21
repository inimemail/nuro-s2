package apicompat

import (
	"encoding/json"
	"net/url"
	"slices"
)

func IncludeWebSearchSources(request *ResponsesRequest) {
	const sources = "web_search_call.action.sources"
	for _, tool := range request.Tools {
		if tool.Type == "web_search" || tool.Type == "web_search_preview" {
			if !slices.Contains(request.Include, sources) {
				request.Include = append(request.Include, sources)
			}
			return
		}
	}
}

func webSearchCallResults(item *ResponsesOutput) json.RawMessage {
	type result struct {
		Type  string `json:"type"`
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	results := make([]result, 0)
	if item.Action == nil {
		return json.RawMessage("[]")
	}
	seen := make(map[string]struct{})
	add := func(raw json.RawMessage) {
		var address string
		if json.Unmarshal(raw, &address) != nil {
			return
		}
		parsed, err := url.Parse(address)
		if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return
		}
		if _, exists := seen[address]; exists {
			return
		}
		seen[address] = struct{}{}
		results = append(results, result{Type: "web_search_result", URL: address, Title: address})
	}
	var sources []json.RawMessage
	if json.Unmarshal(item.Action.Sources, &sources) == nil && sources != nil {
		for _, raw := range sources {
			var source struct {
				URL json.RawMessage `json:"url"`
			}
			if json.Unmarshal(raw, &source) == nil {
				add(source.URL)
			}
		}
	} else if item.Action.Type == "open_page" {
		add(item.Action.URL)
	}
	encoded, _ := json.Marshal(results)
	return encoded
}
