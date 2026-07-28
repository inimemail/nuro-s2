package service

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

var errAntigravityProjectIDRequired = errors.New("antigravity project_id is required")

const antigravityProjectIDFallbackCredentialKey = "project_id"

func resolveAntigravityProjectID(account *Account) (string, error) {
	if account == nil {
		return "", errAntigravityProjectIDRequired
	}
	if value := strings.TrimSpace(account.GetCredential("project_id")); value != "" {
		return value, nil
	}
	if value := strings.TrimSpace(account.GetExtraString("project_id")); value != "" {
		return value, nil
	}
	return "", errAntigravityProjectIDRequired
}

func (s *AntigravityGatewayService) readUpstreamErrorBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	return func() []byte {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		return body
	}()
}

func ApplyThinkingEnabledFallback(effort *string, body []byte, mappedModel string) *string {
	if effort != nil || !OpenAIBodyHasThinkingEnabled(body) {
		return effort
	}
	// Keep this conservative: only fill the usage dimension for models that
	// already accept reasoning, and never rewrite request JSON.
	if strings.TrimSpace(mappedModel) == "" {
		return nil
	}
	value := "high"
	return &value
}

func OpenAIBodyHasThinkingEnabled(body []byte) bool {
	// Query the top-level field without decoding the whole request. This keeps
	// the compatibility check off the request transformation path while being
	// insensitive to whitespace, field order, and additional thinking fields.
	typeName := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	return typeName == "enabled" || typeName == "adaptive"
}
