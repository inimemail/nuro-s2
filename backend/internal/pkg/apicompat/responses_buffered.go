package apicompat

import (
	"encoding/json"
	"slices"
	"strings"
)

type bufferedFuncCall struct {
	Type                  string
	ID                    string
	CallID                string
	Name                  string
	Namespace             string
	EncryptedFunctionArgs json.RawMessage
	EncryptedContent      string
	Args                  strings.Builder
}

// BufferedResponseAccumulator collects content from Responses SSE delta events
// so that non-streaming handlers can reconstruct output when the terminal event
// (response.completed / response.done) carries an empty output array.
type BufferedResponseAccumulator struct {
	funcCalls            []*bufferedFuncCall
	outputIndexToFuncIdx map[int]int
	completedItems       map[int]ResponsesOutput
	messageParts         map[int]map[int]*bufferedMessagePart
	reasoningParts       map[int]map[int]*strings.Builder
	rawReasoningParts    map[int]map[int]*strings.Builder
}

// NewBufferedResponseAccumulator returns an initialised accumulator.
func NewBufferedResponseAccumulator() *BufferedResponseAccumulator {
	return &BufferedResponseAccumulator{
		outputIndexToFuncIdx: make(map[int]int),
	}
}

// ProcessEvent inspects a single Responses SSE event and accumulates any
// content it carries. Only delta events that contribute to the final output
// are handled; all other event types are silently ignored.
func (a *BufferedResponseAccumulator) ProcessEvent(event *ResponsesStreamEvent) {
	if event == nil || !validBufferedIndex(event.OutputIndex) {
		return
	}
	switch event.Type {
	case "response.refusal.delta":
		a.appendMessagePart(event, "refusal")
	case "response.refusal.done":
		a.appendMessagePart(event, "refusal")
	case "response.output_text.delta":
		a.appendMessagePart(event, "output_text")
	case "response.output_text.done":
		a.appendMessagePart(event, "output_text")
	case "response.content_part.done":
		if event.Part != nil && (event.Part.Type == "output_text" || event.Part.Type == "refusal") {
			a.appendMessagePart(event, event.Part.Type)
		}
	case "response.output_item.added", "response.output_item.done":
		if event.Type == "response.output_item.done" && event.Item != nil {
			item := *event.Item
			if item.Status == "" {
				item.Status = "completed"
			}
			switch item.Type {
			case "message", "reasoning", "web_search_call", "function_call", "custom_tool_call", "mcp_call", "tool_search_call":
				if a.completedItems == nil {
					a.completedItems = make(map[int]ResponsesOutput)
				}
				a.completedItems[event.OutputIndex] = item
			}
		}
		if event.Item != nil && (event.Item.Type == "function_call" || event.Item.Type == "custom_tool_call") {
			idx, exists := a.outputIndexToFuncIdx[event.OutputIndex]
			if !exists {
				idx = len(a.funcCalls)
				a.outputIndexToFuncIdx[event.OutputIndex] = idx
				// Builders must stay at a stable address when this slice grows.
				a.funcCalls = append(a.funcCalls, &bufferedFuncCall{})
			}
			call := a.funcCalls[idx]
			call.Type = event.Item.Type
			if event.Item.ID != "" {
				call.ID = event.Item.ID
			}
			if event.Item.CallID != "" {
				call.CallID = event.Item.CallID
			}
			if event.Item.Name != "" {
				call.Name = event.Item.Name
			}
			if event.Item.Namespace != "" {
				call.Namespace = event.Item.Namespace
			}
			if event.Item.EncryptedContent != "" {
				call.EncryptedContent = event.Item.EncryptedContent
			}
			if event.Item.EncryptedFunctionArgs != nil {
				call.EncryptedFunctionArgs = append(json.RawMessage(nil), event.Item.EncryptedFunctionArgs...)
			}
			// The done item is authoritative and may be the only arguments source.
			arguments := event.Item.Arguments
			if event.Item.Type == "custom_tool_call" {
				arguments = event.Item.Input
			}
			if arguments != "" {
				call.Args.Reset()
				_, _ = call.Args.WriteString(arguments)
			}
		}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if event.Delta != "" {
			if idx, ok := a.outputIndexToFuncIdx[event.OutputIndex]; ok {
				_, _ = a.funcCalls[idx].Args.WriteString(event.Delta)
			}
		}
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		if idx, ok := a.outputIndexToFuncIdx[event.OutputIndex]; ok {
			value := event.Arguments
			if event.Type == "response.custom_tool_call_input.done" {
				value = event.Input
			}
			if value != "" {
				a.funcCalls[idx].Args.Reset()
				_, _ = a.funcCalls[idx].Args.WriteString(value)
			}
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		a.appendReasoningPart(event)
	case "response.reasoning_text.delta":
		a.appendReasoningPart(event)
	}
}

// HasContent reports whether any content has been accumulated.
func (a *BufferedResponseAccumulator) HasContent() bool {
	return len(a.messageParts) > 0 || len(a.reasoningParts) > 0 ||
		len(a.rawReasoningParts) > 0 || len(a.funcCalls) > 0 || len(a.completedItems) > 0
}

// BuildOutput constructs the reconstructed output for callers with no terminal body.
func (a *BufferedResponseAccumulator) BuildOutput() []ResponsesOutput {
	var response ResponsesResponse
	a.SupplementResponseOutput(&response)
	return response.Output
}

type bufferedMessagePart struct {
	kind string
	text strings.Builder
}

func validBufferedIndex(index int) bool {
	return index >= 0 && index < 1024
}

func (a *BufferedResponseAccumulator) appendMessagePart(event *ResponsesStreamEvent, kind string) {
	if !validBufferedIndex(event.ContentIndex) {
		return
	}
	text := event.Delta
	if event.Part != nil {
		text = event.Part.Text
		if kind == "refusal" {
			text = event.Part.Refusal
		}
	} else if event.Text != "" {
		text = event.Text
	}
	if kind == "refusal" && event.Refusal != "" {
		text = event.Refusal
	}
	if text == "" {
		return
	}
	if a.messageParts == nil {
		a.messageParts = make(map[int]map[int]*bufferedMessagePart)
	}
	parts := a.messageParts[event.OutputIndex]
	if parts == nil {
		parts = make(map[int]*bufferedMessagePart)
		a.messageParts[event.OutputIndex] = parts
	}
	part := parts[event.ContentIndex]
	if part == nil || part.kind != kind {
		part = &bufferedMessagePart{kind: kind}
		parts[event.ContentIndex] = part
	}
	if event.Delta == "" {
		part.text.Reset()
	}
	_, _ = part.text.WriteString(text)
}

func (a *BufferedResponseAccumulator) appendReasoningPart(event *ResponsesStreamEvent) {
	if !validBufferedIndex(event.SummaryIndex) || (event.Delta == "" && event.Text == "") {
		return
	}
	byOutput := &a.reasoningParts
	if event.Type == "response.reasoning_text.delta" {
		byOutput = &a.rawReasoningParts
	}
	if *byOutput == nil {
		*byOutput = make(map[int]map[int]*strings.Builder)
	}
	parts := (*byOutput)[event.OutputIndex]
	if parts == nil {
		parts = make(map[int]*strings.Builder)
		(*byOutput)[event.OutputIndex] = parts
	}
	part := parts[event.SummaryIndex]
	if part == nil {
		part = &strings.Builder{}
		parts[event.SummaryIndex] = part
	}
	if event.Text != "" {
		part.Reset()
		_, _ = part.WriteString(event.Text)
	} else {
		_, _ = part.WriteString(event.Delta)
	}
}

func sortedBufferedIndexes[V any](items map[int]V) []int {
	indexes := make([]int, 0, len(items))
	for index := range items {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	return indexes
}

// SupplementResponseOutput uses terminal output when present. Otherwise it
// fills confirmed slots in order, using full items before their deltas.
func (a *BufferedResponseAccumulator) SupplementResponseOutput(resp *ResponsesResponse) {
	if resp == nil || len(resp.Output) > 0 || (resp.Status != "" && resp.Status != "completed" && resp.Status != "done") {
		return
	}
	slots := make(map[int]struct{})
	for index := range a.completedItems {
		slots[index] = struct{}{}
	}
	for index := range a.messageParts {
		slots[index] = struct{}{}
	}
	for index := range a.reasoningParts {
		slots[index] = struct{}{}
	}
	for index := range a.rawReasoningParts {
		slots[index] = struct{}{}
	}
	for index := range a.outputIndexToFuncIdx {
		slots[index] = struct{}{}
	}
	seen := make(map[string]bool)
	appendItem := func(item ResponsesOutput) {
		if item.Type == "" || item.Status == "failed" || item.Status == "incomplete" {
			return
		}
		if (item.ID != "" && seen["item:"+item.ID]) || (item.CallID != "" && seen["call:"+item.CallID]) {
			return
		}
		if item.ID != "" {
			seen["item:"+item.ID] = true
		}
		if item.CallID != "" {
			seen["call:"+item.CallID] = true
		}
		item.rebuiltFromStream = true
		resp.Output = append(resp.Output, item)
	}
	for _, index := range sortedBufferedIndexes(slots) {
		item := a.completedItems[index]
		if item.Type == "" || item.Type == "reasoning" {
			reasoning := item
			if len(reasoning.Summary) == 0 {
				parts := a.reasoningParts[index]
				if len(parts) == 0 {
					parts = a.rawReasoningParts[index]
				}
				for _, part := range sortedBufferedIndexes(parts) {
					reasoning.Summary = append(reasoning.Summary, ResponsesSummary{Type: "summary_text", Text: parts[part].String()})
				}
			}
			if len(reasoning.Summary) > 0 || reasoning.EncryptedContent != "" {
				reasoning.Type = "reasoning"
				appendItem(reasoning)
			}
		}
		if item.Type == "" || item.Type == "message" {
			message := item
			if len(message.Content) == 0 {
				parts := a.messageParts[index]
				for _, partIndex := range sortedBufferedIndexes(parts) {
					part := parts[partIndex]
					content := ResponsesContentPart{Type: part.kind, Text: part.text.String()}
					if part.kind == "refusal" {
						content.Text, content.Refusal = "", content.Text
					}
					message.Content = append(message.Content, content)
				}
			}
			if len(message.Content) > 0 {
				message.Type, message.Role = "message", "assistant"
				appendItem(message)
			}
		}
		if callIndex, ok := a.outputIndexToFuncIdx[index]; ok {
			call := a.funcCalls[callIndex]
			if item.Type != "function_call" && item.Type != "custom_tool_call" {
				if item.Type != "" {
					if item.Type != "message" && item.Type != "reasoning" {
						appendItem(item)
					}
					continue
				}
				item.Type = call.Type
			}
			if item.ID == "" {
				item.ID = call.ID
			}
			if item.CallID == "" {
				item.CallID = call.CallID
			}
			if item.Name == "" {
				item.Name = call.Name
			}
			if item.Namespace == "" {
				item.Namespace = call.Namespace
			}
			if item.EncryptedContent == "" {
				item.EncryptedContent = call.EncryptedContent
			}
			if item.EncryptedFunctionArgs == nil {
				item.EncryptedFunctionArgs = call.EncryptedFunctionArgs
			}
			if item.Type == "custom_tool_call" {
				if item.Input == "" {
					item.Input = call.Args.String()
				}
			} else if item.Arguments == "" {
				item.Arguments = call.Args.String()
			}
			appendItem(item)
		} else if item.Type != "message" && item.Type != "reasoning" {
			appendItem(item)
		}
	}
}
