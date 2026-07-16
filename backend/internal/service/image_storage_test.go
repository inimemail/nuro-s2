package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

type imageStorageRecorder struct {
	key, contentType string
	data             []byte
}

func (s *imageStorageRecorder) Save(_ context.Context, key, contentType string, data []byte) (string, error) {
	s.key, s.contentType, s.data = key, contentType, append([]byte(nil), data...)
	return "https://cdn.example/" + key, nil
}

func TestImageResultUploaderOffloadsBase64BeforePersistence(t *testing.T) {
	storage := &imageStorageRecorder{}
	uploader := NewImageResultUploader(storage, "images/", 1024)
	encoded := base64.StdEncoding.EncodeToString([]byte("small-image"))
	result, err := uploader.Rewrite(context.Background(), "imgtask_1", []byte(`{"data":[{"b64_json":"`+encoded+`","revised_prompt":"cat"}],"usage":{"output_tokens":1}}`))
	require.NoError(t, err)
	require.NotContains(t, string(result), "b64_json")
	require.Contains(t, string(result), "https://cdn.example/images/imgtask_1-0.png")
	require.Equal(t, []byte("small-image"), storage.data)
	var parsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(result, &parsed))
	require.Contains(t, parsed, "usage")
}

func TestImageResultUploaderRejectsOversizedBase64(t *testing.T) {
	uploader := NewImageResultUploader(&imageStorageRecorder{}, "", 2)
	encoded := base64.StdEncoding.EncodeToString([]byte("too large"))
	_, err := uploader.Rewrite(context.Background(), "imgtask_1", []byte(`{"data":[{"b64_json":"`+encoded+`"}]}`))
	require.Error(t, err)
}
