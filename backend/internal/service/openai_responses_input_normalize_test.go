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
