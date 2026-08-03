package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const defaultImageStorageMaxBytes int64 = 32 << 20

type ImageStorage interface {
	Save(ctx context.Context, key, contentType string, data []byte) (string, error)
}

type ImageResultUploader struct {
	storage  ImageStorage
	prefix   string
	maxBytes int64
}

func NewImageResultUploader(storage ImageStorage, prefix string, maxBytes int64) *ImageResultUploader {
	if maxBytes <= 0 {
		maxBytes = defaultImageStorageMaxBytes
	}
	return &ImageResultUploader{storage: storage, prefix: strings.TrimLeft(prefix, "/"), maxBytes: maxBytes}
}

func (u *ImageResultUploader) Enabled() bool {
	return u != nil && u.storage != nil
}

// Rewrite offloads inline base64 images. Existing upstream URLs remain
// untouched, so task completion never introduces an untrusted server-side download.
func (u *ImageResultUploader) Rewrite(ctx context.Context, taskID string, result []byte) ([]byte, error) {
	if !u.Enabled() {
		return nil, errors.New("image object storage is unavailable")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(result, &top); err != nil {
		return nil, fmt.Errorf("parse image result: %w", err)
	}
	rawData, ok := top["data"]
	if !ok {
		return append([]byte(nil), result...), nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(rawData, &items); err != nil {
		return nil, fmt.Errorf("parse image result data: %w", err)
	}
	changed := false
	for i, item := range items {
		data, contentType, shouldOffload, err := imageResultInlineData(item, u.maxBytes)
		if err != nil {
			return nil, fmt.Errorf("image %d: %w", i, err)
		}
		if !shouldOffload {
			continue
		}
		if int64(len(data)) > u.maxBytes {
			return nil, fmt.Errorf("image %d exceeds object storage limit", i)
		}
		key := u.prefix + taskID + "-" + strconv.Itoa(i) + storedImageExtension(contentType)
		url, err := u.storage.Save(ctx, key, contentType, data)
		if err != nil {
			return nil, fmt.Errorf("store image %d: %w", i, err)
		}
		item["url"], _ = json.Marshal(url)
		delete(item, "b64_json")
		items[i] = item
		changed = true
	}
	if !changed {
		return append([]byte(nil), result...), nil
	}
	top["data"], _ = json.Marshal(items)
	return json.Marshal(top)
}

// imageResultInlineData extracts the two inline formats used by OpenAI image
// task responses: b64_json and data:image/* URLs. Remote URLs are deliberately
// left untouched because downloading arbitrary upstream content here would
// turn task completion into a server-side request primitive.
func imageResultInlineData(item map[string]json.RawMessage, maxBytes int64) ([]byte, string, bool, error) {
	if raw, ok := item["b64_json"]; ok {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil || strings.TrimSpace(encoded) == "" {
			return nil, "", false, errors.New("invalid b64_json")
		}
		data, err := decodeImageBase64(encoded, maxBytes)
		if err != nil {
			return nil, "", false, fmt.Errorf("decode b64_json: %w", err)
		}
		contentType, err := validateStoredImageContentType(data)
		if err != nil {
			return nil, "", false, fmt.Errorf("validate b64_json: %w", err)
		}
		return data, contentType, true, nil
	}
	if raw, ok := item["url"]; ok {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, "", false, nil
		}
		data, contentType, ok, err := decodeImageDataURL(value, maxBytes)
		if err != nil {
			return nil, "", false, err
		}
		if ok {
			contentType, err = validateStoredImageContentType(data)
			if err != nil {
				return nil, "", false, fmt.Errorf("validate image data URL: %w", err)
			}
		}
		return data, contentType, ok, nil
	}
	return nil, "", false, nil
}

func decodeImageBase64(encoded string, maxBytes int64) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, errors.New("empty base64 payload")
	}
	if maxBytes <= 0 {
		maxBytes = defaultImageStorageMaxBytes
	}
	decode := func(encoding *base64.Encoding) ([]byte, error) {
		decoder := base64.NewDecoder(encoding, strings.NewReader(encoded))
		data, err := io.ReadAll(io.LimitReader(decoder, maxBytes+1))
		if int64(len(data)) > maxBytes {
			return nil, fmt.Errorf("decoded image exceeds object storage limit of %d bytes", maxBytes)
		}
		return data, err
	}
	data, err := decode(base64.StdEncoding)
	if err == nil {
		return data, nil
	}
	// Raw encoding is only valid without padding. Do not trim arbitrary
	// padding, otherwise malformed values such as "====" become empty data.
	if strings.Contains(encoded, "=") {
		return nil, err
	}
	return decode(base64.RawStdEncoding)
}

func decodeImageDataURL(value string, maxBytes int64) ([]byte, string, bool, error) {
	if !strings.HasPrefix(strings.ToLower(value), "data:image/") {
		return nil, "", false, nil
	}
	comma := strings.IndexByte(value, ',')
	if comma <= len("data:") {
		return nil, "", false, errors.New("invalid image data URL")
	}
	meta, payload := value[len("data:"):comma], value[comma+1:]
	parts := strings.Split(meta, ";")
	contentType := strings.ToLower(strings.TrimSpace(parts[0]))
	base64Encoded := false
	for _, part := range parts[1:] {
		if strings.EqualFold(strings.TrimSpace(part), "base64") {
			base64Encoded = true
		}
	}
	if !base64Encoded || !strings.HasPrefix(contentType, "image/") {
		return nil, "", false, errors.New("image data URL must use base64 encoding")
	}
	if strings.TrimSpace(payload) == "" {
		return nil, "", false, errors.New("image data URL has empty payload")
	}
	data, err := decodeImageBase64(payload, maxBytes)
	if err != nil {
		return nil, "", false, fmt.Errorf("decode data URL: %w", err)
	}
	return data, contentType, true, nil
}

func validateStoredImageContentType(data []byte) (string, error) {
	contentType := strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0])
	if !strings.HasPrefix(contentType, "image/") {
		return "", errors.New("decoded payload is not an image")
	}
	return contentType, nil
}

func storedImageExtension(contentType string) string {
	switch {
	case strings.Contains(contentType, "jpeg"), strings.Contains(contentType, "jpg"):
		return ".jpg"
	case strings.Contains(contentType, "webp"):
		return ".webp"
	case strings.Contains(contentType, "gif"):
		return ".gif"
	default:
		return ".png"
	}
}
