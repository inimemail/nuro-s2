package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
		raw, ok := item["b64_json"]
		if !ok {
			continue
		}
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil || strings.TrimSpace(encoded) == "" {
			return nil, fmt.Errorf("image %d has invalid b64_json", i)
		}
		if int64(base64.StdEncoding.DecodedLen(len(encoded))) > u.maxBytes {
			return nil, fmt.Errorf("image %d exceeds object storage limit", i)
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode image %d: %w", i, err)
		}
		if int64(len(data)) > u.maxBytes {
			return nil, fmt.Errorf("image %d exceeds object storage limit", i)
		}
		contentType := detectStoredImageContentType(data)
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

func detectStoredImageContentType(data []byte) string {
	contentType := strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0])
	if strings.HasPrefix(contentType, "image/") {
		return contentType
	}
	return "image/png"
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
