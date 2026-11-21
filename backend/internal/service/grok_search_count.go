package service

import (
	"math"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
)

// countGrokNativeSearchCallsFromJSONBytes counts completed native search tool
// calls in a Responses-style JSON body (output array or nested response.output).
// Counts: web_search_call, x_search_call, tool_search_call, and function_call
// named tool_search / web_search / x_search.
func countGrokNativeSearchCallsFromJSONBytes(body []byte) int {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return 0
	}
	for _, path := range []string{"response.usage", "usage"} {
		if count, ok := grokSearchCountFromReportedUsage(gjson.GetBytes(body, path)); ok {
			return count
		}
	}
	// Responses envelopes normally expose either top-level output (JSON mode)
	// or response.output (terminal SSE payload). Compatibility layers can retain
	// both copies; counting both would bill the same search twice. Prefer the
	// canonical nested response when present and fall back to top-level output.
	if nested := gjson.GetBytes(body, "response.output"); nested.IsArray() {
		return countGrokNativeSearchCallsInOutputArray(nested)
	}
	return countGrokNativeSearchCallsInOutputArray(gjson.GetBytes(body, "output"))
}

func countGrokNativeSearchCallsFromSSEBody(body string) int {
	if strings.TrimSpace(body) == "" {
		return 0
	}
	var search grokSearchStreamCount
	forEachOpenAISSEDataPayload(body, func(data []byte) {
		search.observe(data)
	})
	return search.count
}

// xAI reports successfully completed searches in the final usage envelope.
func grokSearchCountFromReportedUsage(usage gjson.Result) (int, bool) {
	details := usage.Get("server_side_tool_usage_details")
	if !details.IsObject() {
		return 0, false
	}
	total := 0
	reported := false
	for _, name := range []string{"web_search_calls", "x_search_calls", "document_search_calls"} {
		value := details.Get(name)
		if !value.Exists() {
			continue
		}
		if value.Type != gjson.Number {
			return 0, false
		}
		count, err := strconv.ParseUint(value.Raw, 10, strconv.IntSize)
		if err != nil || count > uint64(math.MaxInt-total) {
			return 0, false
		}
		reported = true
		total += int(count)
	}
	return total, reported
}

type grokSearchStreamCount struct {
	count    int
	reported bool
	seen     map[string]struct{}
}

func (s *grokSearchStreamCount) observe(payload []byte) {
	if s.reported {
		return
	}
	switch strings.TrimSpace(gjson.GetBytes(payload, "type").String()) {
	case "response.completed", "response.incomplete", "response.done":
		for _, path := range []string{"response.usage", "usage"} {
			if count, ok := grokSearchCountFromReportedUsage(gjson.GetBytes(payload, path)); ok && gjson.ValidBytes(payload) {
				s.count = count
				s.reported = true
				return
			}
		}
	case "response.output_item.done", "":
	default:
		return
	}
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	s.count += countGrokNativeSearchCallsInSSEDataDedup(payload, s.seen)
}

// countGrokNativeSearchCallsInSSEDataDedup accounts for completed output
// items once across item.done and the terminal copy.
func countGrokNativeSearchCallsInSSEDataDedup(data []byte, seen map[string]struct{}) int {
	keys := collectGrokNativeSearchCallKeys(data)
	if len(keys) == 0 {
		return 0
	}
	added := 0
	isItemDone := strings.TrimSpace(gjson.GetBytes(data, "type").String()) == "response.output_item.done"
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			if !isItemDone || !strings.HasPrefix(key, "synth:") {
				continue
			}
			// Legacy item.done events without a call ID are counted in order.
			base := key[:strings.LastIndexByte(key, ':')]
			for ordinal := 2; ; ordinal++ {
				candidate := base + ":" + strconv.Itoa(ordinal)
				if _, used := seen[candidate]; !used {
					key = candidate
					break
				}
			}
		}
		seen[key] = struct{}{}
		added++
	}
	return added
}

func collectGrokNativeSearchCallKeys(data []byte) []string {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return nil
	}
	// An empty type means a bare item object without an SSE envelope; anything
	// else that is not a completion event carries no billable call.
	switch strings.TrimSpace(gjson.GetBytes(data, "type").String()) {
	case "response.output_item.done", "response.completed", "response.incomplete", "response.done", "":
	default:
		return nil
	}
	var keys []string
	syntheticOrdinals := make(map[string]int)
	consider := func(item gjson.Result) {
		if !isGrokNativeSearchOutputItem(item) {
			return
		}
		key := grokSearchCallID(item)
		if key == "" {
			// Include the ordinal among same-kind calls. A plain type:name key
			// collapses two id-less web searches in one completed response into
			// one charge. The ordinal remains stable between ordered item.done
			// events and response.completed output.
			base := "synth:" + strings.ToLower(strings.TrimSpace(item.Get("type").String())) +
				":" + strings.ToLower(strings.TrimSpace(item.Get("name").String()))
			syntheticOrdinals[base]++
			key = base + ":" + strconv.Itoa(syntheticOrdinals[base])
		} else {
			key = "id:" + key
		}
		keys = append(keys, key)
	}
	if item := gjson.GetBytes(data, "item"); item.Exists() {
		consider(item)
	} else if output := gjson.GetBytes(data, "response.output"); output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			consider(item)
			return true
		})
	} else if output := gjson.GetBytes(data, "output"); output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			consider(item)
			return true
		})
	} else {
		consider(gjson.ParseBytes(data))
	}
	return keys
}

func grokSearchCallID(item gjson.Result) string {
	return firstNonEmpty(
		strings.TrimSpace(item.Get("call_id").String()),
		strings.TrimSpace(item.Get("id").String()),
		strings.TrimSpace(item.Get("item.call_id").String()),
		strings.TrimSpace(item.Get("item.id").String()),
	)
}

func countGrokNativeSearchCallsInOutputArray(output gjson.Result) int {
	if !output.IsArray() {
		return 0
	}
	count := 0
	var seen map[string]struct{}
	output.ForEach(func(_, item gjson.Result) bool {
		if !isGrokNativeSearchOutputItem(item) {
			return true
		}
		if id := grokSearchCallID(item); id != "" {
			if _, exists := seen[id]; exists {
				return true
			}
			if seen == nil {
				seen = make(map[string]struct{})
			}
			seen[id] = struct{}{}
		}
		count++
		return true
	})
	return count
}

func isGrokNativeSearchOutputItem(item gjson.Result) bool {
	if !item.Exists() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("status").String())) {
	case "in_progress", "searching", "queued", "pending", "failed", "incomplete", "cancelled", "canceled":
		return false
	}
	itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
	if apicompat.IsServerSideSearchCall(itemType, item.Get("call_id").String()) {
		return true
	}
	switch itemType {
	case "web_search_call", "x_search_call", "tool_search_call":
		return true
	case "function_call", "custom_tool_call":
		name := strings.ToLower(strings.TrimSpace(item.Get("name").String()))
		return name == "web_search" || name == "x_search" || name == "tool_search"
	default:
		return false
	}
}
