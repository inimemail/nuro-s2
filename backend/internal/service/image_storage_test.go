package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"
)

func encodedTestImage(t *testing.T, format string) (string, []byte) {
	t.Helper()
	var buffer bytes.Buffer
	pixel := image.NewRGBA(image.Rect(0, 0, 1, 1))
	pixel.Set(0, 0, color.RGBA{R: 255, A: 255})
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&buffer, pixel, nil)
	} else {
		err = png.Encode(&buffer, pixel)
	}
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(buffer.Bytes()), buffer.Bytes()
}

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
	encoded, imageBytes := encodedTestImage(t, "png")
	result, err := uploader.Rewrite(context.Background(), "imgtask_1", []byte(`{"data":[{"b64_json":"`+encoded+`","revised_prompt":"cat"}],"usage":{"output_tokens":1}}`))
	require.NoError(t, err)
	require.NotContains(t, string(result), "b64_json")
	require.Contains(t, string(result), "https://cdn.example/images/imgtask_1-0.png")
	require.Equal(t, imageBytes, storage.data)
	require.Equal(t, "image/png", storage.contentType)
	var parsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(result, &parsed))
	require.Contains(t, parsed, "usage")
}

func TestImageResultUploaderRejectsOversizedBase64(t *testing.T) {
	storage := &imageStorageRecorder{}
	uploader := NewImageResultUploader(storage, "", 2)
	encoded := base64.StdEncoding.EncodeToString([]byte("too large"))
	_, err := uploader.Rewrite(context.Background(), "imgtask_1", []byte(`{"data":[{"b64_json":"`+encoded+`"}]}`))
	require.ErrorContains(t, err, "object storage limit")
	require.Empty(t, storage.data)

	_, err = uploader.Rewrite(context.Background(), "imgtask_2", []byte(`{"data":[{"url":"data:image/png;base64,`+encoded+`"}]}`))
	require.ErrorContains(t, err, "object storage limit")
	require.Empty(t, storage.data)
}

func TestImageResultUploaderOffloadsBase64DataURL(t *testing.T) {
	storage := &imageStorageRecorder{}
	uploader := NewImageResultUploader(storage, "images/", 2048)
	encoded, imageBytes := encodedTestImage(t, "jpeg")
	result, err := uploader.Rewrite(context.Background(), "imgtask_data", []byte(`{"data":[{"url":"data:image/jpeg;base64,`+encoded+`"}]}`))
	require.NoError(t, err)
	require.Contains(t, string(result), "https://cdn.example/images/imgtask_data-0.jpg")
	require.NotContains(t, string(result), "data:image")
	require.Equal(t, "image/jpeg", storage.contentType)
	require.Equal(t, imageBytes, storage.data)
}

func TestImageResultUploaderLeavesRemoteURLUntouched(t *testing.T) {
	uploader := NewImageResultUploader(&imageStorageRecorder{}, "images/", 1024)
	input := []byte(`{"data":[{"url":"https://upstream.example/image.png"}]}`)
	result, err := uploader.Rewrite(context.Background(), "imgtask_remote", input)
	require.NoError(t, err)
	require.JSONEq(t, string(input), string(result))
}

func TestImageResultUploaderRejectsMalformedBase64Padding(t *testing.T) {
	uploader := NewImageResultUploader(&imageStorageRecorder{}, "images/", 1024)
	_, err := uploader.Rewrite(context.Background(), "imgtask_invalid", []byte(`{"data":[{"b64_json":"===="}]}`))
	require.Error(t, err)
}

func TestImageResultUploaderRejectsDecodedNonImagePayloads(t *testing.T) {
	storage := &imageStorageRecorder{}
	uploader := NewImageResultUploader(storage, "images/", 1024)
	encoded := base64.StdEncoding.EncodeToString([]byte("not-an-image"))

	_, err := uploader.Rewrite(context.Background(), "imgtask_invalid_b64", []byte(`{"data":[{"b64_json":"`+encoded+`"}]}`))
	require.ErrorContains(t, err, "decoded payload is not an image")
	require.Empty(t, storage.data)

	_, err = uploader.Rewrite(context.Background(), "imgtask_spoofed_data_url", []byte(`{"data":[{"url":"data:image/png;base64,`+encoded+`"}]}`))
	require.ErrorContains(t, err, "decoded payload is not an image")
	require.Empty(t, storage.data)
}
