package service

import (
	"math"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Search within the smallest labeled container that has usage. Never ascend
// into a container holding both windows, and ignore script/style contents.
func parseOllamaLabeledWindows(raw string) (*OllamaCloudUsageWindow, *OllamaCloudUsageWindow) {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return nil, nil
	}
	aliases := [][]string{{"session usage", "5 hour usage", "5-hour usage", "5h usage", "5 hour limit", "5-hour limit"}, {"weekly usage", "7 day usage", "7-day usage", "7d usage", "weekly limit", "7 day limit"}}
	var result [2]*OllamaCloudUsageWindow
	hasLabel := func(text string, labels []string) bool {
		for _, label := range labels {
			if strings.Contains(text, label) {
				return true
			}
		}
		return false
	}
	// Each subtree contributes at most 601 bytes. Avoid rescanning and copying
	// large ancestor subtrees for every element in a provider-controlled page.
	var visit func(*html.Node) string
	visit = func(n *html.Node) string {
		if n.Data == "script" || n.Data == "style" {
			return ""
		}
		if n.Type == html.TextNode {
			text := strings.Join(strings.Fields(n.Data), " ")
			return text[:min(len(text), 601)]
		}
		var b strings.Builder
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			text := visit(child)
			if text != "" && b.Len() < 601 {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(text[:min(len(text), 601-b.Len())])
			}
		}
		text := b.String()
		if n.Type != html.ElementNode {
			return text
		}
		if len(text) > 600 {
			return text
		}
		lower := strings.ToLower(text)
		for i, labels := range aliases {
			if !hasLabel(lower, labels) || hasLabel(lower, aliases[1-i]) {
				continue
			}
			matches := ollamaPercentRE.FindAllStringSubmatch(text, -1)
			if len(matches) != 1 {
				continue
			}
			percent, err := strconv.ParseFloat(matches[0][1], 64)
			if err != nil || math.IsInf(percent, 0) {
				continue
			}
			if strings.Contains(lower, "remaining") && !strings.Contains(lower, "used") {
				percent = 100 - percent
			}
			if percent < 0 {
				continue
			}
			window := &OllamaCloudUsageWindow{UsedPercent: percent}
			if match := ollamaResetRE.FindStringSubmatch(text); len(match) == 3 {
				value, err := strconv.ParseFloat(match[1], 64)
				unit := strings.ToLower(match[2])
				multiplier := time.Minute
				if strings.HasPrefix(unit, "hour") || strings.HasPrefix(unit, "hr") {
					multiplier = time.Hour
				}
				if strings.HasPrefix(unit, "day") {
					multiplier = 24 * time.Hour
				}
				duration := value * float64(multiplier)
				if err == nil && duration > 0 && duration <= float64(366*24*time.Hour) {
					reset := time.Now().UTC().Add(time.Duration(duration))
					window.ResetAt, window.ResetText = &reset, match[0]
				}
			}
			if result[i] == nil || result[i].ResetAt == nil {
				result[i] = window
			}
		}
		return text
	}
	visit(doc)
	return result[0], result[1]
}
