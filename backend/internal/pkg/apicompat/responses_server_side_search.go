package apicompat

import "strings"

// Only explicit Grok callers use the xAI server-owned search prefixes.
func IsServerSideSearchCall(itemType, callID string) bool {
	return itemType == "custom_tool_call" && (strings.HasPrefix(callID, "xs_") || strings.HasPrefix(callID, "as_"))
}

func withoutGrokServerSearch(resp *ResponsesResponse) *ResponsesResponse {
	if resp == nil {
		return nil
	}
	var output []ResponsesOutput
	for i, item := range resp.Output {
		if IsServerSideSearchCall(item.Type, item.CallID) {
			if output == nil {
				output = make([]ResponsesOutput, 0, len(resp.Output))
				output = append(output, resp.Output[:i]...)
			}
		} else if output != nil {
			output = append(output, item)
		}
	}
	if output == nil {
		return resp
	}
	clone := *resp
	clone.Output = output
	return &clone
}

func filterGrokSearchEvent(evt *ResponsesStreamEvent, enabled bool, ignored *map[int]bool) *ResponsesStreamEvent {
	if !enabled || evt == nil {
		return evt
	}
	if evt.Item != nil && IsServerSideSearchCall(evt.Item.Type, evt.Item.CallID) {
		if *ignored == nil {
			*ignored = make(map[int]bool)
		}
		(*ignored)[evt.OutputIndex] = true
		return nil
	}
	switch evt.Type {
	case "response.output_item.added", "response.output_item.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "response.function_call_arguments.delta", "response.function_call_arguments.done":
		if (*ignored)[evt.OutputIndex] {
			return nil
		}
	}
	if filtered := withoutGrokServerSearch(evt.Response); filtered != evt.Response {
		clone := *evt
		clone.Response = filtered
		return &clone
	}
	return evt
}
