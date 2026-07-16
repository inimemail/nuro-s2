package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	raw := []byte(`{"name":"account","password":"pw","credentials":{"access_token":"token-value","base_url":"https://example.invalid"},"items":[{"apiKey":"secret"}]}`)
	redacted := RedactAuditBody(raw, "application/json")
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(redacted), &got))
	require.Equal(t, "***", got["password"])
	credentials := got["credentials"].(map[string]any)
	require.Equal(t, "***", credentials["access_token"])
	require.Equal(t, "https://example.invalid", credentials["base_url"])
	require.NotContains(t, redacted, "token-value")
	require.NotContains(t, redacted, "secret")
}

func TestRedactAuditBodyOmitsUnsafeAndOversizedBodies(t *testing.T) {
	require.Equal(t, "<non-json body omitted>", RedactAuditBody([]byte("password=secret"), "application/x-www-form-urlencoded"))
	result := RedactAuditBody([]byte(strings.Repeat("x", AuditRequestBodyCaptureLimit+1)), "application/json")
	require.Contains(t, result, "body omitted")
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
