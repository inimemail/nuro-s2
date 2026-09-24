package apicompat

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// This is an opaque transport envelope, not encryption. Anthropic still checks
// the original signature. Scope binds it to the selected account/model and ID
// binds the signed block to its original Responses item.
const anthropicThinkingEnvelopePrefix = "anthropic-thinking-v1:"
const maxAnthropicThinkingEnvelope = 2 << 20

type anthropicThinkingEnvelope struct {
	Scope     string `json:"scope"`
	ItemID    string `json:"item_id"`
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

func encodeAnthropicThinking(block AnthropicContentBlock, scope, id string) string {
	if scope == "" {
		return ""
	}
	envelope := anthropicThinkingEnvelope{"", id, block.Type, block.Thinking, block.Signature, block.Data}
	envelope.Scope = thinkingEnvelopeMAC(envelope, scope)
	raw, err := json.Marshal(envelope)
	if err != nil || len(raw) > maxAnthropicThinkingEnvelope {
		return ""
	}
	return anthropicThinkingEnvelopePrefix + base64.RawStdEncoding.EncodeToString(raw)
}

func decodeAnthropicThinking(value, scope, id string) (AnthropicContentBlock, error) {
	fail := func() (AnthropicContentBlock, error) {
		return AnthropicContentBlock{}, fmt.Errorf("invalid or mismatched Anthropic thinking envelope")
	}
	if scope == "" {
		return fail()
	}
	if !strings.HasPrefix(value, anthropicThinkingEnvelopePrefix) {
		return fail()
	}
	encoded := strings.TrimPrefix(value, anthropicThinkingEnvelopePrefix)
	if len(encoded) > base64.RawStdEncoding.EncodedLen(maxAnthropicThinkingEnvelope) {
		return fail()
	}
	raw, err := base64.RawStdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return fail()
	}
	var envelope anthropicThinkingEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil {
		return fail()
	}
	if decoder.Decode(new(any)) != io.EOF || envelope.ItemID != id {
		return fail()
	}
	mac := envelope.Scope
	envelope.Scope = ""
	expectedMAC := thinkingEnvelopeMAC(envelope, scope)
	envelope.Scope = mac
	if !hmac.Equal([]byte(mac), []byte(expectedMAC)) {
		return fail()
	}
	// A canonical encoding also rejects duplicate object fields and hidden data.
	canonical, _ := json.Marshal(envelope)
	if !bytes.Equal(canonical, raw) {
		return fail()
	}
	switch envelope.Type {
	case "thinking":
		if envelope.Signature == "" || envelope.Data != "" {
			return fail()
		}
	case "redacted_thinking":
		if envelope.Data == "" || envelope.Signature != "" || envelope.Thinking != "" {
			return fail()
		}
	default:
		return fail()
	}
	return AnthropicContentBlock{Type: envelope.Type, Thinking: envelope.Thinking, Signature: envelope.Signature, Data: envelope.Data}, nil
}

func thinkingEnvelopeMAC(envelope anthropicThinkingEnvelope, scope string) string {
	envelope.Scope = ""
	raw, _ := json.Marshal(envelope)
	mac := hmac.New(sha256.New, []byte(scope))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}
