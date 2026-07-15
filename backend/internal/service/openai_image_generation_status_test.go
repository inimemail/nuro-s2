package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeCompletedImageGenerationStatus(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		want        string
		wantChanged bool
	}{
		{
			name:        "completed output item",
			input:       `{"type":"response.output_item.done","item":{"type":"image_generation_call","status":"generating","result":"image-data"}}`,
			want:        `{"type":"response.output_item.done","item":{"type":"image_generation_call","status":"completed","result":"image-data"}}`,
			wantChanged: true,
		},
		{
			name:        "completed response",
			input:       `{"type":"response.completed","response":{"output":[{"type":"image_generation_call","status":"in_progress","result":"image-data"}]}}`,
			want:        `{"type":"response.completed","response":{"output":[{"type":"image_generation_call","status":"completed","result":"image-data"}]}}`,
			wantChanged: true,
		},
		{
			name:        "non-stream response object",
			input:       `{"object":"response","status":"completed","output":[{"type":"image_generation_call","status":"generating","result":"image-data"}]}`,
			want:        `{"object":"response","status":"completed","output":[{"type":"image_generation_call","status":"completed","result":"image-data"}]}`,
			wantChanged: true,
		},
		{
			name:        "incomplete response remains incomplete",
			input:       `{"type":"response.incomplete","response":{"output":[{"type":"image_generation_call","status":"generating","result":"partial-data"}]}}`,
			want:        `{"type":"response.incomplete","response":{"output":[{"type":"image_generation_call","status":"generating","result":"partial-data"}]}}`,
			wantChanged: false,
		},
		{
			name:        "empty result remains generating",
			input:       `{"type":"response.done","response":{"output":[{"type":"image_generation_call","status":"generating","result":""}]}}`,
			want:        `{"type":"response.done","response":{"output":[{"type":"image_generation_call","status":"generating","result":""}]}}`,
			wantChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := normalizeCompletedImageGenerationStatus([]byte(tt.input))
			require.Equal(t, tt.wantChanged, changed)
			require.JSONEq(t, tt.want, string(got))
		})
	}
}
