package xai

// IncludeIndependentReasoningTokens adds reasoning tokens only when total_tokens
// proves they are not already included in output_tokens. xAI may report visible
// output and reasoning separately, while OpenAI reports inclusive output.
func IncludeIndependentReasoningTokens(input, output, total, reasoning int64) int64 {
	if input < 0 || output < 0 || reasoning <= 0 || total <= 0 {
		return output
	}
	if total == input+output {
		return output
	}
	gap := total - input - output
	if gap <= 0 {
		return output
	}
	if reasoning < gap {
		gap = reasoning
	}
	return output + gap
}
