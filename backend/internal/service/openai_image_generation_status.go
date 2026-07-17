package service

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeCompletedImageGenerationStatus fixes upstream events that carry a
// completed image result while leaving status at generating/in_progress.
// Only terminal image items are changed; non-terminal progress events remain
// untouched.
func normalizeCompletedImageGenerationStatus(data []byte) ([]byte, bool) {
	// Most streaming frames are text deltas. Avoid a full JSON validation on
	// the first-token path unless the frame can actually contain an image item.
	if len(data) == 0 || !bytes.Contains(data, []byte("image_generation_call")) || !gjson.ValidBytes(data) {
		return data, false
	}
	shouldNormalize := func(item gjson.Result) bool {
		if !item.Exists() || !item.IsObject() || strings.TrimSpace(item.Get("type").String()) != "image_generation_call" {
			return false
		}
		switch strings.TrimSpace(item.Get("status").String()) {
		case "generating", "in_progress":
			return strings.TrimSpace(item.Get("result").String()) != ""
		default:
			return false
		}
	}
	normalizeOutput := func(input []byte, outputPath string, output gjson.Result) ([]byte, bool) {
		if !output.Exists() || !output.IsArray() {
			return input, false
		}
		updated := input
		changed := false
		for i, item := range output.Array() {
			if !shouldNormalize(item) {
				continue
			}
			next, err := sjson.SetBytes(updated, outputPath+"."+strconv.Itoa(i)+".status", "completed")
			if err != nil {
				return input, false
			}
			updated, changed = next, true
		}
		return updated, changed
	}

	switch strings.TrimSpace(gjson.GetBytes(data, "type").String()) {
	case "response.output_item.done":
		if !shouldNormalize(gjson.GetBytes(data, "item")) {
			return data, false
		}
		updated, err := sjson.SetBytes(data, "item.status", "completed")
		return updated, err == nil
	case "response.completed", "response.done":
		return normalizeOutput(data, "response.output", gjson.GetBytes(data, "response.output"))
	default:
		// Non-streaming Responses returns the response object directly, without
		// the SSE event wrapper. It can still carry a completed image result with
		// a stale generating/in_progress item status.
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "status").String())) {
		case "completed", "done":
			return normalizeOutput(data, "output", gjson.GetBytes(data, "output"))
		default:
			return data, false
		}
	}
}
