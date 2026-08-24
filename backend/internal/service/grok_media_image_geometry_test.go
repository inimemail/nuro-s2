package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestApplyGrokImagineImageGeometry(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		resolution string
		aspect     string
	}{
		{name: "square 1k", body: `{"size":"1024x1024"}`, resolution: "1k", aspect: "1:1"},
		{name: "wide 2k", body: `{"size":"2048x1152"}`, resolution: "2k", aspect: "16:9"},
		{name: "preserve explicit", body: `{"size":"1024x1024","resolution":"2k","aspect_ratio":"9:16"}`, resolution: "2k", aspect: "9:16"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyGrokImagineImageGeometry([]byte(tt.body))
			require.NoError(t, err)
			require.False(t, gjson.GetBytes(got, "size").Exists())
			require.Equal(t, tt.resolution, gjson.GetBytes(got, "resolution").String())
			require.Equal(t, tt.aspect, gjson.GetBytes(got, "aspect_ratio").String())
		})
	}
}

func TestOpenAIUsageFromGJSONIncludesIndependentGrokReasoning(t *testing.T) {
	usage, ok := openAIUsageFromGJSON(gjson.Parse(`{"input_tokens":32,"output_tokens":9,"total_tokens":151,"output_tokens_details":{"reasoning_tokens":110}}`))
	require.True(t, ok)
	require.Equal(t, 119, usage.OutputTokens)

	usage, ok = openAIUsageFromGJSON(gjson.Parse(`{"input_tokens":32,"output_tokens":119,"total_tokens":151,"output_tokens_details":{"reasoning_tokens":110}}`))
	require.True(t, ok)
	require.Equal(t, 119, usage.OutputTokens)
}
