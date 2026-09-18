package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// Both buffered and streaming bridges require a real terminal event. An SSE
// error or an interrupted body must never be finalized as a successful turn.
func (s *OpenAIGatewayService) scanNativeAnthropicEvents(resp *http.Response, emit func(*apicompat.AnthropicStreamEvent) error) error {
	defer resp.Body.Close()
	limit := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		limit = s.cfg.Gateway.MaxLineSize
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), limit)
	pump := newAnthropicNativeLinePump(scanner, s.anthropicNativeStreamInterval())
	defer pump.stop()
	var data strings.Builder
	eventName := ""
	started, completed := false, false
	toolArguments := make(map[int]string)
	toolDeltas := make(map[int]bool)
	blockTypes := make(map[int]string)
	closedBlocks := make(map[int]bool)
	openBlock := -1
	validateArguments := func() error {
		for _, args := range toolArguments {
			if args != "" && !json.Valid([]byte(args)) {
				return errors.New("invalid upstream Anthropic tool arguments")
			}
		}
		return nil
	}
	dispatch := func() error {
		if data.Len() == 0 {
			eventName = ""
			return nil
		}
		payload := data.String()
		data.Reset()
		var event apicompat.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("invalid upstream Anthropic event: %w", err)
		}
		if event.Type == "" {
			event.Type = eventName
		}
		eventName = ""
		if event.Type == "error" {
			return fmt.Errorf("upstream Anthropic error: %s", sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage([]byte(payload))))
		}
		if event.Type == "message_start" {
			if started || event.Message == nil {
				return errors.New("invalid upstream Anthropic message_start")
			}
			started = true
		}
		switch event.Type {
		case "content_block_start":
			if openBlock >= 0 {
				return errors.New("upstream Anthropic content blocks overlap")
			}
			if !started || event.Index == nil || *event.Index < 0 || event.ContentBlock == nil {
				return errors.New("invalid upstream Anthropic content block")
			}
			if _, exists := blockTypes[*event.Index]; exists {
				return errors.New("duplicate upstream Anthropic content block")
			}
			blockTypes[*event.Index] = event.ContentBlock.Type
			openBlock = *event.Index
			if event.ContentBlock.Type == "tool_use" {
				toolArguments[*event.Index] = string(event.ContentBlock.Input)
			}
		case "content_block_delta":
			if event.Index == nil || event.Delta == nil {
				return errors.New("invalid upstream Anthropic content delta")
			}
			blockType, exists := blockTypes[*event.Index]
			if !exists || closedBlocks[*event.Index] {
				return errors.New("upstream Anthropic delta without open block")
			}
			if (event.Delta.Type == "input_json_delta" && blockType != "tool_use") ||
				(event.Delta.Type == "text_delta" && blockType != "text") ||
				((event.Delta.Type == "thinking_delta" || event.Delta.Type == "signature_delta") && blockType != "thinking") {
				return errors.New("upstream Anthropic delta does not match block type")
			}
			if event.Delta.Type == "input_json_delta" && event.Delta.PartialJSON != "" {
				idx := *event.Index
				if _, ok := toolArguments[idx]; !ok {
					return errors.New("upstream tool delta without tool block")
				}
				if !toolDeltas[idx] {
					toolArguments[idx] = ""
					toolDeltas[idx] = true
				}
				toolArguments[idx] += event.Delta.PartialJSON
			}
		case "content_block_stop":
			if event.Index == nil {
				return errors.New("upstream Anthropic block stop without index")
			}
			if _, exists := blockTypes[*event.Index]; !exists || closedBlocks[*event.Index] {
				return errors.New("upstream Anthropic stop without open block")
			}
			closedBlocks[*event.Index] = true
			openBlock = -1
			args, ok := toolArguments[*event.Index]
			if ok && args != "" && !json.Valid([]byte(args)) {
				return errors.New("invalid upstream Anthropic tool arguments")
			}
		}
		if event.Type == "message_stop" {
			if openBlock >= 0 {
				return errors.New("upstream Anthropic stopped before content_block_stop")
			}
			if !started {
				return errors.New("upstream Anthropic stopped before message_start")
			}
			if err := validateArguments(); err != nil {
				return err
			}
			completed = true
		}
		return emit(&event)
	}
	for {
		line, err := pump.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("read upstream Anthropic stream: %w", err)
			}
			if err := dispatch(); err != nil {
				return err
			}
			if completed {
				return nil
			}
			return errors.New("upstream Anthropic stream ended before message_stop")
		}
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			if completed {
				return nil
			}
			continue
		}
		if name, ok := extractOpenAISSEEventLine(line); ok {
			eventName = name
			continue
		}
		if payload, ok := extractOpenAISSEDataLine(line); ok {
			if data.Len()+len(payload)+1 > limit {
				return errors.New("upstream Anthropic event exceeds size limit")
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(payload)
		}
	}
}

func (s *OpenAIGatewayService) readNativeAnthropicResponse(resp *http.Response) (*apicompat.AnthropicResponse, ClaudeUsage, error) {
	var result *apicompat.AnthropicResponse
	var usage ClaudeUsage
	blocks := make(map[int]int)
	argumentDeltas := make(map[int]bool)
	err := s.scanNativeAnthropicEvents(resp, func(event *apicompat.AnthropicStreamEvent) error {
		switch event.Type {
		case "message_start":
			result = event.Message
			mergeAnthropicUsage(&usage, result.Usage)
		case "message_delta":
			if event.Usage != nil {
				mergeAnthropicUsage(&usage, *event.Usage)
			}
			if result != nil && event.Delta != nil && event.Delta.StopReason != "" {
				result.StopReason = apicompat.AnthropicStopReasonPtr(event.Delta.StopReason)
			}
		case "content_block_start":
			if result == nil || event.Index == nil || *event.Index < 0 || event.ContentBlock == nil {
				return errors.New("invalid upstream Anthropic content block")
			}
			if _, exists := blocks[*event.Index]; exists {
				return errors.New("duplicate upstream Anthropic content block")
			}
			blocks[*event.Index] = len(result.Content)
			result.Content = append(result.Content, *event.ContentBlock)
		case "content_block_delta":
			if result == nil || event.Index == nil || event.Delta == nil {
				return errors.New("invalid upstream Anthropic content delta")
			}
			idx, ok := blocks[*event.Index]
			if !ok {
				return errors.New("upstream Anthropic delta without block")
			}
			block := &result.Content[idx]
			switch event.Delta.Type {
			case "text_delta":
				block.Text += event.Delta.Text
			case "thinking_delta":
				block.Thinking += event.Delta.Thinking
			case "signature_delta":
				block.Signature += event.Delta.Signature
			case "input_json_delta":
				if event.Delta.PartialJSON == "" {
					break
				}
				// input:{} is a start placeholder, not an argument fragment.
				if !argumentDeltas[*event.Index] {
					block.Input = nil
					argumentDeltas[*event.Index] = true
				}
				block.Input = appendRawJSON(block.Input, event.Delta.PartialJSON)
			}
		}
		return nil
	})
	if err != nil {
		return nil, usage, err
	}
	if result == nil {
		return nil, usage, errors.New("upstream stream ended without response")
	}
	for _, block := range result.Content {
		if block.Type == "tool_use" && len(block.Input) > 0 && !json.Valid(block.Input) {
			return nil, usage, errors.New("invalid upstream Anthropic tool arguments")
		}
	}
	result.Usage = apicompat.AnthropicUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CacheCreationInputTokens: usage.CacheCreationInputTokens, CacheReadInputTokens: usage.CacheReadInputTokens}
	return result, usage, nil
}
