package service

import "strings"

func isRejectedOpenAIEncryptedReasoning(code, message string) bool {
	return code == "invalid_encrypted_content" ||
		(code == "thinking_signature_invalid" &&
			strings.Contains(message, "The encrypted content") &&
			strings.Contains(message, "could not be verified") &&
			strings.Contains(message, "could not be decrypted or parsed"))
}
