package service

import (
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"
	"github.com/tidwall/gjson"
)

const maxOpenAIHTTPErrorMetadataBytes = 64 << 10

// openAIHTTPErrorMetadata recognizes protocol categories and known parameter
// names without exposing provider-supplied messages or extension fields.
func openAIHTTPErrorMetadata(status int, body []byte) (code, param, responseID string) {
	if len(body) > maxOpenAIHTTPErrorMetadataBytes || !utf8.Valid(body) || !gjson.ValidBytes(body) {
		return "", "", ""
	}
	root := gjson.ParseBytes(body)
	errorObject := root.Get("error")
	id := root.Get("response_id")
	if !errorObject.IsObject() {
		errorObject = root.Get("response.error")
		id = root.Get("response.id")
	}
	if !errorObject.IsObject() {
		return "", "", ""
	}
	if id.Type == gjson.String && strings.HasPrefix(id.Str, "resp_") &&
		len(id.Str) > len("resp_") && id.Str == strings.TrimSpace(id.Str) && openAIErrorIDIsSafe(id.Str) {
		responseID = id.Str
	}
	message := errorObject.Get("message")
	value := errorObject.Get("code")
	if (!value.Exists() || value.Type == gjson.Null || (value.Type == gjson.String && strings.TrimSpace(value.Str) == "")) &&
		message.Type == gjson.String && gjson.Valid(message.Str) {
		errorObject = gjson.Parse(message.Str).Get("error")
		value = errorObject.Get("code")
		message = errorObject.Get("message")
	}
	switch candidate := errorObject.Get("param").Str; candidate {
	case "input", "encrypted_content", "instructions", "previous_response_id", "messages":
		param = candidate
	}
	if value.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(value.Str)) {
		case "server_is_overloaded", "slow_down":
			return "server_error", "", responseID
		case "invalid_encrypted_content":
			if status == http.StatusBadRequest {
				return "invalid_encrypted_content", param, responseID
			}
		case "thinking_signature_invalid":
			text := strings.ToLower(message.Str)
			if status == http.StatusBadRequest && strings.Contains(text, "encrypted content") &&
				(strings.Contains(text, "could not be verified") || strings.Contains(text, "could not be decrypted")) {
				return "invalid_encrypted_content", param, responseID
			}
		case "context_length_exceeded", "context_too_large":
			if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
				return "context_length_exceeded", param, responseID
			}
		}
	}
	if (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) && isOpenAIContextWindowError(message.Str, nil) {
		return "context_length_exceeded", param, responseID
	}
	return "", "", responseID
}

// WriteOpenAICompactCapacityFailure handles the final failure after retries
// have been exhausted and a compact keepalive has committed the HTTP status.
func WriteOpenAICompactCapacityFailure(c *gin.Context, status, upstreamStatus int, body []byte, errType, model string) bool {
	if !openAICompactClientWantsStream(c) {
		return false
	}
	code, _, _ := openAIHTTPErrorMetadata(upstreamStatus, body)
	if code != "server_error" || !StopOpenAICompactSSEKeepaliveCommitted(c) {
		return false
	}
	writeOpenAIResponsesFailureSSE(c, status, errType, code, "Upstream capacity is temporarily unavailable", "", model)
	return true
}

func writeOpenAIHTTPPassthroughError(c *gin.Context, account *Account, status int, body []byte, model string) {
	code, param, responseID := openAIHTTPErrorMetadata(status, body)
	if responseID != "" && !openAIPassthroughRuleMessageIsSafe(account, responseID) {
		responseID = ""
	}
	errType, message := "upstream_error", safeUpstreamErrorMessage
	switch code {
	case "invalid_encrypted_content":
		errType, message = "invalid_request_error", "The encrypted content could not be verified"
	case "context_length_exceeded":
		errType, message = "invalid_request_error", "The request exceeds the model's context window"
	case "server_error":
		errType, message = "server_error", "Upstream capacity is temporarily unavailable"
	}
	if status != http.StatusRequestEntityTooLarge {
		if svc := getBoundErrorPassthroughService(c); svc != nil {
			if rule := svc.matchLocalRule(account.Platform, status, body); rule != nil {
				mappedStatus, mappedMessage := projectErrorPassthroughRule(c, rule, status, message)
				if mappedStatus >= 400 && mappedStatus <= 599 {
					status = mappedStatus
				}
				if !rule.PassthroughBody && rule.CustomMessage != nil &&
					!containsSavedAccountSecret(account, *rule.CustomMessage) && openAIPassthroughRuleMessageIsSafe(account, mappedMessage) {
					message = mappedMessage
				}
			}
		}
	}

	errorBody := gin.H{"type": errType, "message": message}
	if code != "" {
		errorBody["code"] = code
	}
	if param != "" {
		errorBody["param"] = param
	}
	response := gin.H{"error": errorBody}
	if model == "" && !openAICompactClientWantsStream(c) && !isOpenAIResponsesRequestPath(c) {
		MarkResponseCommitted(c)
		writeOpenAIErrorJSON(c, status, response)
		return
	}
	withOpenAIPlaceholderWriterLock(c, func(writer gin.ResponseWriter) {
		compactCommitted := openAICompactClientWantsStream(c) && StopOpenAICompactSSEKeepaliveCommitted(c)
		queueSSE := c.GetBool("gateway_queue_sse_committed")
		responsesCommitted := writer.Written() && (queueSSE ||
			strings.HasPrefix(strings.ToLower(writer.Header().Get("Content-Type")), "text/event-stream"))
		if compactCommitted || responsesCommitted || model != "" {
			if model != "" && responseID == "" {
				responseID = "resp_placeholder_failed"
			}
			writeOpenAIResponsesFailureSSELocked(c, writer, status, "upstream_error", firstNonEmptyString(code, errType), message, responseID, model)
			return
		}
		if writer.Written() || !claimGatewayErrorTerminal(c) {
			return
		}
		if coordinator := openAIPlaceholderCoordinatorFromContext(c); coordinator != nil {
			coordinator.mu.Lock()
			coordinator.terminalWritten = true
			coordinator.pendingTimeoutFrame = nil
			coordinator.stopOnce.Do(func() { close(coordinator.stop) })
			coordinator.mu.Unlock()
		}
		MarkResponseCommitted(c)
		writer.WriteHeader(status)
		if status < 200 || status == http.StatusNoContent || status == http.StatusNotModified {
			(render.JSON{}).WriteContentType(writer)
			writer.WriteHeaderNow()
			return
		}
		if err := (render.JSON{Data: response}).Render(writer); err != nil {
			_ = c.Error(err)
			c.Abort()
		}
	})
}

func openAIPassthroughRuleMessageIsSafe(account *Account, message string) bool {
	if message == "" || len(message) > 1024 || !utf8.ValidString(message) || redactOpsPlainTextSecrets(message) != message {
		return false
	}
	for _, r := range message {
		if unicode.Is(unicode.Bidi_Control, r) || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') {
			return false
		}
	}
	lower := strings.ToLower(message)
	if name := strings.TrimSpace(account.Name); name != "" && strings.Contains(lower, strings.ToLower(name)) {
		return false
	}
	if target, err := url.Parse(strings.TrimSpace(account.GetCredential("base_url"))); err == nil {
		if host := strings.TrimSuffix(strings.ToLower(target.Hostname()), "."); host != "" && strings.Contains(lower, host) {
			return false
		}
	}
	return !containsSavedAccountSecret(account, message)
}

func containsSavedAccountSecret(account *Account, text string) bool {
	for _, key := range SensitiveCredentialKeys {
		if secret := strings.TrimSpace(account.GetCredential(key)); secret != "" && strings.Contains(text, secret) {
			return true
		}
	}
	// Read the saved values without populating the account's forwarding cache.
	for name, value := range stringMappingFromRaw(account.Credentials[credKeyHeaderOverrides]) {
		if value = strings.TrimSpace(value); value != "" && (text == value ||
			((len(value) >= 8 || isSensitiveKey(strings.ReplaceAll(name, "-", "_"))) && strings.Contains(text, value))) {
			return true
		}
	}
	if proxy := account.Proxy; proxy != nil {
		if proxy.Password != "" && strings.Contains(text, proxy.Password) {
			return true
		}
		if proxy.Username != "" && strings.Contains(text, proxy.Username) {
			return true
		}
	}
	return false
}
