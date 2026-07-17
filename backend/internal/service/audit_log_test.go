package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

type auditRetentionRepoStub struct {
	AuditLogRepository
	deleteCalls int
	cutoff      time.Time
}

func (r *auditRetentionRepoStub) DeleteBefore(_ context.Context, cutoff time.Time, _ int) (int64, error) {
	r.deleteCalls++
	r.cutoff = cutoff
	if r.deleteCalls == 1 {
		return 5000, nil
	}
	return 0, nil
}

func TestRedactAuditBodyRecursive(t *testing.T) {
	raw := []byte(`{"name":"account","password":"pw","verify_code":"123456","invitation_code":"invite","pkey":"merchant-secret","apiV3Key":"wx-secret","credentials":{"access_token":"token-value","agent_private_key":"private-value","base_url":"https://example.invalid"},"items":[{"apiKey":"secret"}]}`)
	redacted := RedactAuditBody(raw, "application/json")
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(redacted), &got))
	require.Equal(t, "***", got["password"])
	credentials := got["credentials"].(map[string]any)
	require.Equal(t, "***", credentials["access_token"])
	require.Equal(t, "https://example.invalid", credentials["base_url"])
	require.NotContains(t, redacted, "token-value")
	require.NotContains(t, redacted, "secret")
	require.NotContains(t, redacted, "123456")
	require.NotContains(t, redacted, "invite")
	require.NotContains(t, redacted, "private-value")
}

func TestRedactAuditBodyOmitsUnsafeAndOversizedBodies(t *testing.T) {
	require.Equal(t, "<non-json body omitted>", RedactAuditBody([]byte("password=secret"), "application/x-www-form-urlencoded"))
	result := RedactAuditBody([]byte(strings.Repeat("x", AuditRequestBodyCaptureLimit+1)), "application/json")
	require.Contains(t, result, "body omitted")
}

func TestRedactAuditBodyTruncatesAtUTF8Boundary(t *testing.T) {
	const jsonPrefix = `{"text":"`
	padding := strings.Repeat("a", auditRequestBodyMaxBytes-len(jsonPrefix)-1)
	raw, err := json.Marshal(map[string]string{"text": padding + "中文"})
	require.NoError(t, err)

	result := RedactAuditBody(raw, "application/json")

	require.True(t, utf8.ValidString(result))
	require.NotContains(t, result, "�")
	require.True(t, strings.HasSuffix(result, "...<truncated>"))
}

func TestMaskAuditCredential(t *testing.T) {
	require.Equal(t, "****", MaskAuditCredential("short"))
	require.Equal(t, "abcdef****wxyz", MaskAuditCredential("abcdefghijklmnopqrstuvwxyz"))
}

func TestParseAuditLogRetentionDays(t *testing.T) {
	require.Equal(t, 180, parseAuditLogRetentionDays(""))
	require.Equal(t, 180, parseAuditLogRetentionDays("invalid"))
	require.Equal(t, 0, parseAuditLogRetentionDays("0"))
	require.Equal(t, 30, parseAuditLogRetentionDays("30"))
}

func TestAuditRetentionOnceDeletesInBatches(t *testing.T) {
	repo := &auditRetentionRepoStub{}
	svc := NewAuditLogService(repo, nil)
	defer svc.cancel()

	svc.runRetentionOnce()

	require.Equal(t, 2, repo.deleteCalls)
	require.WithinDuration(t, time.Now().UTC().AddDate(0, 0, -180), repo.cutoff, time.Minute)
}
