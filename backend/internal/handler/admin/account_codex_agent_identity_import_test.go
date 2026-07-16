package admin

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNormalizeCodexImportEntryAcceptsAgentIdentityAuthJSON(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	privateKeyBase64 := base64.StdEncoding.EncodeToString(der)

	item, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: map[string]any{
		"auth_mode": "agentIdentity",
		"agent_identity": map[string]any{
			"agent_runtime_id":  "runtime-import",
			"agent_private_key": privateKeyBase64,
			"account_id":        "account-import",
			"chatgpt_user_id":   "user-import",
			"email":             "agent@example.invalid",
			"plan_type":         "pro",
		},
	}})
	require.NoError(t, err)
	require.True(t, item.IsAgentIdentity)
	require.Equal(t, service.OpenAIAuthModeAgentIdentity, item.Credentials["auth_mode"])
	require.Equal(t, privateKeyBase64, item.Credentials["agent_private_key"])
	require.NotContains(t, item.Credentials, "access_token")
	require.NotContains(t, item.Credentials, "refresh_token")
	require.NotEmpty(t, item.WarningTexts)

	expiresAt, credentialExpiresAt, autoPause, warnings, err := resolveCodexImportExpiry(CodexSessionImportRequest{}, item)
	require.NoError(t, err)
	require.Nil(t, expiresAt)
	require.Nil(t, credentialExpiresAt)
	require.Nil(t, autoPause)
	require.Empty(t, warnings)
}

func TestNormalizeCodexImportEntryRejectsInvalidAgentPrivateKey(t *testing.T) {
	_, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: map[string]any{
		"auth_mode":         "agentIdentity",
		"agent_runtime_id":  "runtime-import",
		"agent_private_key": base64.StdEncoding.EncodeToString([]byte("not-pkcs8")),
		"account_id":        "account-import",
		"chatgpt_user_id":   "user-import",
	}})
	require.Error(t, err)
}
