package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func shouldSanitizeOpenAIResponsesInputItemIDs(account *Account, passthroughEnabled bool) bool {
	return !passthroughEnabled && account != nil &&
		account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey
}

// Invalid replay IDs are deleted rather than rewritten: a fabricated msg/fc
// ID could refer to a different upstream object.
func shouldStripOpenAIResponsesInputItemID(itemType, id string) bool {
	if id == "" {
		return false
	}
	if itemType == "message" {
		return !strings.HasPrefix(id, "msg")
	}
	if itemType == "reasoning" {
		return !strings.HasPrefix(id, "rs")
	}
	if isCodexToolCallInputType(itemType) {
		return !strings.HasPrefix(id, "fc")
	}
	return false
}

func sanitizeOpenAIResponsesInputItemIDs(body []byte) ([]byte, bool, error) {
	// A read-only view avoids copying the entire image-bearing input array.
	input := gjson.Parse(openAIWSPayloadStringView(body)).Get("input")
	if !input.IsArray() {
		return body, false, nil
	}

	items := make([]string, 0)
	changed := false
	var sanitizeErr error
	index := 0
	input.ForEach(func(_, item gjson.Result) bool {
		currentIndex := index
		index++
		itemBody := item.Raw
		if item.IsObject() {
			itemType := item.Get("type")
			id := item.Get("id")
			if itemType.Type == gjson.String && id.Type == gjson.String &&
				shouldStripOpenAIResponsesInputItemID(itemType.String(), id.String()) {
				itemBody, sanitizeErr = sjson.Delete(itemBody, "id")
				if sanitizeErr != nil {
					sanitizeErr = fmt.Errorf("delete input.%d.id: %w", currentIndex, sanitizeErr)
					return false
				}
				changed = true
			}
		}
		items = append(items, itemBody)
		return true
	})
	if sanitizeErr != nil {
		return nil, false, sanitizeErr
	}
	if !changed {
		return body, false, nil
	}

	size := len(body) - len(input.Raw) + 2
	for i, item := range items {
		size += len(item)
		if i > 0 {
			size++
		}
	}
	rebuilt := make([]byte, 0, size)
	rebuilt = append(rebuilt, body[:input.Index]...)
	rebuilt = append(rebuilt, '[')
	for index, item := range items {
		if index > 0 {
			rebuilt = append(rebuilt, ',')
		}
		rebuilt = append(rebuilt, item...)
	}
	rebuilt = append(rebuilt, ']')
	rebuilt = append(rebuilt, body[input.Index+len(input.Raw):]...)
	return rebuilt, true, nil
}
