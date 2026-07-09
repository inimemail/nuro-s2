package service

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAIResponsesStringInputBody(t *testing.T) {
	t.Run("string_input", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":"hi"}`)
		normalized, changed, err := normalizeOpenAIResponsesStringInputBody(body)
		if err != nil {
			t.Fatalf("normalize string input: %v", err)
		}
		if !changed {
			t.Fatal("expected string input to be normalized")
		}
		if !gjson.GetBytes(normalized, "input").IsArray() {
			t.Fatalf("expected input array, got %s", string(normalized))
		}
		if got := gjson.GetBytes(normalized, "input.0.content.0.text").String(); got != "hi" {
			t.Fatalf("unexpected input text: %q", got)
		}
	})

	t.Run("array_input_is_unchanged", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":"hi"}]}`)
		normalized, changed, err := normalizeOpenAIResponsesStringInputBody(body)
		if err != nil {
			t.Fatalf("normalize array input: %v", err)
		}
		if changed {
			t.Fatalf("expected array input to be unchanged, got %s", string(normalized))
		}
		if string(normalized) != string(body) {
			t.Fatalf("expected original body to be returned, got %s", string(normalized))
		}
	})

	t.Run("preserves_blank_string", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":"   "}`)
		normalized, changed, err := normalizeOpenAIResponsesStringInputBody(body)
		if err != nil {
			t.Fatalf("normalize blank string input: %v", err)
		}
		if !changed {
			t.Fatal("expected blank string input to be normalized")
		}
		if got := gjson.GetBytes(normalized, "input.0.content.0.text").String(); got != "   " {
			t.Fatalf("expected blank input to be preserved, got %q", got)
		}
	})
}

func TestNormalizeOpenAIAPIKeyResponsesUnsupportedParamsBody(t *testing.T) {
	t.Run("strips_token_params", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","max_output_tokens":128,"max_completion_tokens":64,"input":[{"type":"message","role":"user","content":"hi"}]}`)
		normalized, changed, err := normalizeOpenAIAPIKeyResponsesUnsupportedParamsBody(body)
		if err != nil {
			t.Fatalf("normalize unsupported params: %v", err)
		}
		if !changed {
			t.Fatal("expected unsupported params to be stripped")
		}
		if gjson.GetBytes(normalized, "max_output_tokens").Exists() {
			t.Fatalf("expected max_output_tokens to be stripped: %s", string(normalized))
		}
		if gjson.GetBytes(normalized, "max_completion_tokens").Exists() {
			t.Fatalf("expected max_completion_tokens to be stripped: %s", string(normalized))
		}
		if !gjson.GetBytes(normalized, "input").IsArray() {
			t.Fatalf("expected input to remain: %s", string(normalized))
		}
	})

	t.Run("unchanged_without_token_params", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":"hi"}]}`)
		normalized, changed, err := normalizeOpenAIAPIKeyResponsesUnsupportedParamsBody(body)
		if err != nil {
			t.Fatalf("normalize unchanged body: %v", err)
		}
		if changed {
			t.Fatalf("expected body to be unchanged, got %s", string(normalized))
		}
		if string(normalized) != string(body) {
			t.Fatalf("expected original body to be returned, got %s", string(normalized))
		}
	})
}

func TestNormalizeOpenAIResponsesInputArgumentsBody(t *testing.T) {
	t.Run("json_object_string_arguments", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":[{"type":"tool_search_call","call_id":"call_1","name":"search","arguments":"{\"query\":\"golang\",\"limit\":2}"},{"type":"message","role":"user","content":"hi"}]}`)
		normalized, changed, err := normalizeOpenAIResponsesInputArgumentsBody(body)
		if err != nil {
			t.Fatalf("normalize input arguments: %v", err)
		}
		if !changed {
			t.Fatal("expected arguments to be normalized")
		}
		if !gjson.GetBytes(normalized, "input.0.arguments").IsObject() {
			t.Fatalf("expected arguments object, got %s", string(normalized))
		}
		if got := gjson.GetBytes(normalized, "input.0.arguments.query").String(); got != "golang" {
			t.Fatalf("unexpected query: %q body=%s", got, string(normalized))
		}
		if got := gjson.GetBytes(normalized, "input.0.arguments.limit").Int(); got != 2 {
			t.Fatalf("unexpected limit: %d body=%s", got, string(normalized))
		}
	})

	t.Run("blank_arguments_unchanged", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":[{"type":"tool_search_call","call_id":"call_1","name":"search","arguments":"   "}]}`)
		normalized, changed, err := normalizeOpenAIResponsesInputArgumentsBody(body)
		if err != nil {
			t.Fatalf("normalize blank arguments: %v", err)
		}
		if changed {
			t.Fatalf("expected blank arguments to stay unchanged, got %s", string(normalized))
		}
		if string(normalized) != string(body) {
			t.Fatalf("expected original body, got %s", string(normalized))
		}
	})

	t.Run("invalid_or_non_object_arguments_unchanged", func(t *testing.T) {
		for _, body := range [][]byte{
			[]byte(`{"model":"gpt-5","input":[{"type":"tool_search_call","arguments":"not-json"}]}`),
			[]byte(`{"model":"gpt-5","input":[{"type":"tool_search_call","arguments":"[1,2]"}]}`),
			[]byte(`{"model":"gpt-5","input":[{"type":"tool_search_call","arguments":"null"}]}`),
			[]byte(`{"model":"gpt-5","input":[{"type":"tool_search_call","arguments":{"query":"golang"}}]}`),
			[]byte(`{"model":"gpt-5","input":[{"type":"function_call","arguments":"{\"cmd\":\"ls\"}"}]}`),
		} {
			normalized, changed, err := normalizeOpenAIResponsesInputArgumentsBody(body)
			if err != nil {
				t.Fatalf("normalize unchanged arguments: %v", err)
			}
			if changed {
				t.Fatalf("expected arguments to stay unchanged, got %s", string(normalized))
			}
			if string(normalized) != string(body) {
				t.Fatalf("expected original body, got %s", string(normalized))
			}
		}
	})
}

func TestNormalizeOpenAIResponsesInputArgumentsMap(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5",
		"input": []any{
			map[string]any{
				"type":      "tool_search_call",
				"call_id":   "call_1",
				"name":      "search",
				"arguments": `{"query":"golang","limit":2}`,
			},
			map[string]any{
				"type":      "function_call",
				"call_id":   "call_2",
				"name":      "noop",
				"arguments": `{"cmd":"ls"}`,
			},
		},
	}

	if !normalizeOpenAIResponsesInputArgumentsMap(reqBody) {
		t.Fatal("expected arguments map normalization")
	}
	input := reqBody["input"].([]any)
	first := input[0].(map[string]any)
	args, ok := first["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("expected first arguments to become object, got %#v", first["arguments"])
	}
	if got := args["query"]; got != "golang" {
		t.Fatalf("unexpected query: %#v", got)
	}
	if got := args["limit"]; got != float64(2) {
		t.Fatalf("unexpected limit: %#v", got)
	}
	second := input[1].(map[string]any)
	if got := second["arguments"]; got != `{"cmd":"ls"}` {
		t.Fatalf("expected function_call arguments to stay string, got %#v", got)
	}
}
