package handler

import "github.com/Wei-Shaw/sub2api/internal/pkg/openai"

func normalizeCodexPageContext(body []byte) ([]byte, bool) {
	return openai.NormalizeCodexPageContext(body)
}
