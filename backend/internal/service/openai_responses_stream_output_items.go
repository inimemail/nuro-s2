package service

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// responsesStreamOutputItems preserves raw output_item.done items so a broken
// terminal event can be repaired without losing item identity or vendor fields.
type responsesStreamOutputItems struct {
	items map[int]json.RawMessage
}

func newResponsesStreamOutputItems() *responsesStreamOutputItems {
	return &responsesStreamOutputItems{items: make(map[int]json.RawMessage)}
}

func (r *responsesStreamOutputItems) Observe(data []byte) {
	if r == nil || len(data) == 0 || !gjson.ValidBytes(data) ||
		strings.TrimSpace(gjson.GetBytes(data, "type").String()) != "response.output_item.done" {
		return
	}
	item := gjson.GetBytes(data, "item")
	if !item.IsObject() {
		return
	}
	index := int(gjson.GetBytes(data, "output_index").Int())
	r.items[index] = json.RawMessage(append([]byte(nil), item.Raw...))
}

func (r *responsesStreamOutputItems) Count() int {
	if r == nil {
		return 0
	}
	return len(r.items)
}

func (r *responsesStreamOutputItems) BuildOutput() ([]byte, bool) {
	if r == nil || len(r.items) == 0 {
		return nil, false
	}
	indexes := make([]int, 0, len(r.items))
	for index := range r.items {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	ordered := make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		ordered = append(ordered, r.items[index])
	}
	encoded, err := json.Marshal(ordered)
	return encoded, err == nil
}

func normalizeResponsesStreamingTerminalOutput(data []byte, doneItems *responsesStreamOutputItems) ([]byte, bool) {
	switch strings.TrimSpace(gjson.GetBytes(data, "type").String()) {
	case "response.completed", "response.done", "response.incomplete", "response.cancelled", "response.canceled":
	default:
		return data, false
	}
	outputJSON, ok := doneItems.BuildOutput()
	if !ok {
		return data, false
	}
	output := gjson.GetBytes(data, "response.output")
	if output.IsArray() && len(output.Array()) >= doneItems.Count() {
		return data, false
	}
	updated, err := sjson.SetRawBytes(data, "response.output", outputJSON)
	if err != nil {
		return data, false
	}
	return updated, true
}
