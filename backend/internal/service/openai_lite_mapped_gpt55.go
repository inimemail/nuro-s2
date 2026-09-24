package service

import (
	"bytes"
	"fmt"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"io"
	"net/http"
	"strings"
)

// Change only the final mapped OAuth request. Keep tool history and namespaces;
// never install GetBody, which would permit implicit transport replay.
func applyMappedGPT55LiteCompatibility(req *http.Request, account *Account, body []byte) error {
	if req == nil || account == nil || !account.IsOpenAIOAuthLike() || strings.TrimSpace(gjson.GetBytes(body, "model").String()) != "gpt-5.5" {
		return nil
	}
	marked := isOpenAIResponsesLiteWebSocketPayload(body)
	if !marked && !isOpenAIResponsesLiteHeader(req.Header.Get(responsesLiteHeader)) {
		return nil
	}
	if marked {
		patched, err := sjson.DeleteBytes(body, "client_metadata."+responsesLiteWSMetadataKey)
		if err != nil {
			return fmt.Errorf("remove mapped GPT-5.5 Lite metadata: %w", err)
		}
		if req.Body != nil {
			_ = req.Body.Close()
		}
		req.Body = io.NopCloser(bytes.NewReader(patched))
		req.ContentLength = int64(len(patched))
	}
	req.GetBody = nil
	req.Header.Del(responsesLiteHeader)
	return nil
}
