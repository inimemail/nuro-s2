package service

import (
	"bytes"
	"context"
	"errors"
	"math"
	"mime/multipart"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/tidwall/gjson"
)

func TestUpgradeV0219GeminiRetryBudget(t *testing.T) {
	if geminiSameAccountMaxAttempts(nil) != 5 {
		t.Fatal("nil config changed default")
	}
	for _, retries := range []int{0, 4, 10} {
		cfg := &config.Config{}
		cfg.Gateway.MaxSameAccountRetriesGemini = &retries
		if got := geminiSameAccountMaxAttempts(cfg); got != retries+1 {
			t.Fatalf("got %d", got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if !errors.Is(sleepGeminiBackoff(ctx, 10), context.Canceled) {
		t.Fatal("cancel not returned")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancel waited for retry timer")
	}
}

func TestUpgradeV0219CacheHitPriorityAndPagination(t *testing.T) {
	if got := openAICacheReadTokensFromUsage(gjson.Parse(`{"prompt_cache_hit_tokens":12}`)); got != 12 {
		t.Fatal(got)
	}
	if got := openAICacheReadTokensFromUsage(gjson.Parse(`{"prompt_tokens_details":{"cached_tokens":5},"prompt_cache_hit_tokens":12}`)); got != 5 {
		t.Fatal(got)
	}
	if got := paginateAPIKeys([]APIKey{{ID: 1}}, pagination.PaginationParams{Page: math.MaxInt, PageSize: 21}); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestUpgradeV0219MalformedToolResultRejected(t *testing.T) {
	for _, body := range []string{`{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}`, `{"response":{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}}`} {
		if _, _, err := antigravity.TransformGeminiToClaude([]byte(body), "model"); !errors.Is(err, antigravity.ErrMalformedFunctionCall) {
			t.Fatal(err)
		}
	}
}

func TestUpgradeV0219MultipartDoesNotTruncate(t *testing.T) {
	for _, size := range []int{openAIImageMaxUploadPartSize - 1, openAIImageMaxUploadPartSize, openAIImageMaxUploadPartSize + 1} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("image", "test.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(bytes.Repeat([]byte{'x'}, size)); err != nil {
			t.Fatal(err)
		}
		if err = writer.Close(); err != nil {
			t.Fatal(err)
		}
		req := &OpenAIImagesRequest{}
		err = parseOpenAIImagesMultipartRequest(body.Bytes(), writer.FormDataContentType(), req)
		if size > openAIImageMaxUploadPartSize {
			if err == nil {
				t.Fatal("oversized image accepted")
			}
		} else if err != nil || len(req.Uploads) != 1 || len(req.Uploads[0].Data) != size {
			t.Fatalf("upload corrupted: %v", err)
		}
		info := ParseGrokMediaRequest(writer.FormDataContentType(), body.Bytes())
		if (info.ParseError != nil) != (size > openAIImageMaxUploadPartSize) {
			t.Fatalf("grok upload boundary: %v", info.ParseError)
		}
	}
}
