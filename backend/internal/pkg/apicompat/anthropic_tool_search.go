package apicompat

import "encoding/json"

const anthropicToolSearchName = "ToolSearch"

// ApplyAnthropicNativeToolSearch promotes the client tool and replays discovered
// schemas in its result items, leaving the top-level tool prefix stable.
func ApplyAnthropicNativeToolSearch(req *AnthropicRequest, out *ResponsesRequest) error {
	if req == nil || out == nil || len(out.Tools) != len(req.Tools) {
		return nil
	}
	deferred := make(map[string]AnthropicTool)
	searchIndex := -1
	for idx, tool := range req.Tools {
		if tool.DeferLoading {
			deferred[tool.Name] = tool
		} else if tool.Name == anthropicToolSearchName {
			if searchIndex >= 0 {
				return nil
			}
			searchIndex = idx
		}
	}
	if searchIndex < 0 {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(req.ToolChoice, &choice) == nil && choice.Type == "tool" {
		if _, hidden := deferred[choice.Name]; hidden || choice.Name == anthropicToolSearchName {
			return nil
		}
	}

	tools := make([]ResponsesTool, 0, len(out.Tools))
	for idx, tool := range req.Tools {
		if tool.DeferLoading {
			continue
		}
		if idx == searchIndex {
			tools = append(tools, ResponsesTool{
				Type: "tool_search", Execution: "client", Description: tool.Description,
				Parameters: normalizeToolParameters(tool.InputSchema),
			})
		} else {
			tools = append(tools, out.Tools[idx])
		}
	}

	history := anthropicToolSearchHistory(req.Messages)
	if len(history) == 0 {
		out.Tools = tools
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(out.Input, &items); err != nil {
		return err
	}
	var pending []ResponsesContentPart
	rewritten := make([]json.RawMessage, 0, len(items))
	flush := func(item *ResponsesInputItem) error {
		if len(pending) == 0 {
			return nil
		}
		if item == nil {
			item = &ResponsesInputItem{Type: "message", Role: "user"}
		} else {
			var parts []ResponsesContentPart
			if err := json.Unmarshal(item.Content, &parts); err != nil {
				return err
			}
			pending = append(pending, parts...)
		}
		content, err := json.Marshal(pending)
		if err != nil {
			return err
		}
		item.Content = content
		wire, err := json.Marshal(item)
		if err != nil {
			return err
		}
		rewritten = append(rewritten, wire)
		pending = nil
		return nil
	}
	for _, raw := range items {
		var item ResponsesInputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		if len(pending) > 0 && item.Type != "function_call_output" {
			if item.Type == "message" && item.Role == "user" {
				if err := flush(&item); err != nil {
					return err
				}
				continue
			}
			if err := flush(nil); err != nil {
				return err
			}
		}
		names, found := history[item.CallID]
		if !found {
			rewritten = append(rewritten, raw)
			continue
		}
		wire := map[string]any{"call_id": item.CallID, "execution": "client", "status": "completed"}
		switch item.Type {
		case "function_call":
			wire["type"] = "tool_search_call"
			wire["arguments"] = anthropicToolSearchArguments(item.Arguments)
		case "function_call_output":
			wire["type"] = "tool_search_output"
			loaded := make([]ResponsesTool, 0, len(names))
			for _, name := range names {
				tool, ok := deferred[name]
				if !ok {
					continue
				}
				loaded = append(loaded, ResponsesTool{
					Type: "function", Name: name, Description: tool.Description, DeferLoading: true,
					Parameters: normalizeToolParameters(tool.InputSchema), Strict: boolPtr(false),
				})
			}
			wire["tools"] = loaded
			if item.Output != "" && item.Output != "(empty)" {
				pending = append(pending, ResponsesContentPart{Type: "input_text", Text: item.Output})
			}
		default:
			rewritten = append(rewritten, raw)
			continue
		}
		encoded, err := json.Marshal(wire)
		if err != nil {
			return err
		}
		rewritten = append(rewritten, encoded)
	}
	if err := flush(nil); err != nil {
		return err
	}
	input, err := json.Marshal(rewritten)
	if err != nil {
		return err
	}
	out.Tools = tools
	out.Input = input
	return nil
}

func anthropicToolSearchHistory(messages []AnthropicMessage) map[string][]string {
	calls := make(map[string][]string)
	for _, message := range messages {
		var blocks []AnthropicContentBlock
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if message.Role == "assistant" && block.Type == "tool_use" && block.Name == anthropicToolSearchName && block.ID != "" {
				calls[toResponsesCallID(block.ID)] = nil
			}
			if message.Role != "user" || block.Type != "tool_result" {
				continue
			}
			callID := toResponsesCallID(block.ToolUseID)
			if _, exists := calls[callID]; !exists {
				continue
			}
			var result []struct {
				Type string `json:"type"`
				Name string `json:"tool_name"`
			}
			if json.Unmarshal(block.Content, &result) != nil {
				continue
			}
			seen := make(map[string]bool)
			for _, ref := range result {
				if ref.Type == "tool_reference" && ref.Name != "" && !seen[ref.Name] {
					seen[ref.Name] = true
					calls[callID] = append(calls[callID], ref.Name)
				}
			}
		}
	}
	return calls
}

func anthropicToolSearchArguments(arguments string) json.RawMessage {
	raw := json.RawMessage(arguments)
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return json.RawMessage("{}")
	}
	return raw
}

func anthropicToolSearchUse(item *ResponsesOutput) AnthropicContentBlock {
	return AnthropicContentBlock{
		Type: "tool_use", ID: fromResponsesCallID(item.CallID), Name: anthropicToolSearchName,
		Input: anthropicToolSearchArguments(item.Arguments),
	}
}
