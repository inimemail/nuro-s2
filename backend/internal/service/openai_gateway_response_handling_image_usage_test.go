package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractOpenAIUsageMergesHostedImageToolUsage(t *testing.T) {
	usage, ok := extractOpenAIUsageFromJSONBytes([]byte(`{
		"response": {
			"usage": {"input_tokens": 11, "output_tokens": 22},
			"tool_usage": {"image_gen": {
				"input_tokens_details": {"image_tokens": 3},
				"output_tokens_details": {"image_tokens": 7}
			}}
		}
	}`))
	require.True(t, ok)
	require.Equal(t, 3, usage.ImageInputTokens)
	require.Equal(t, 7, usage.ImageOutputTokens)
}

func TestExtractOpenAIUsageKeepsPrimaryImageUsage(t *testing.T) {
	usage, ok := extractOpenAIUsageFromJSONBytes([]byte(`{
		"usage": {
			"input_tokens": 11,
			"output_tokens": 22,
			"input_tokens_details": {"image_tokens": 5},
			"output_tokens_details": {"image_tokens": 9}
		},
		"tool_usage": {"image_gen": {
			"input_tokens_details": {"image_tokens": 3},
			"output_tokens_details": {"image_tokens": 7}
		}}
	}`))
	require.True(t, ok)
	require.Equal(t, 5, usage.ImageInputTokens)
	require.Equal(t, 9, usage.ImageOutputTokens)
}
