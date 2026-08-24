package xai

import "testing"

func TestIncludeIndependentReasoningTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                         string
		input, output, total, reason int64
		want                         int64
	}{
		{name: "xAI chat", input: 32, output: 9, total: 135, reason: 94, want: 103},
		{name: "xAI responses", input: 32, output: 9, total: 151, reason: 110, want: 119},
		{name: "OpenAI inclusive", input: 32, output: 103, total: 135, reason: 94, want: 103},
		{name: "gap smaller than reasoning", input: 10, output: 20, total: 33, reason: 5, want: 23},
		{name: "no total", input: 10, output: 20, total: 0, reason: 5, want: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IncludeIndependentReasoningTokens(tt.input, tt.output, tt.total, tt.reason); got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}
