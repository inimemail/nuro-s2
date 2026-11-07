package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

type exportRepoFixture struct {
	UsageLogRepository
	calls    int
	boundary int64
	after    *UsageExportPosition
}

func (r *exportRepoFixture) ExportUsagePage(_ context.Context, p pagination.PaginationParams, f usagestats.UsageLogFilters, high int64, after *UsageExportPosition) ([]UsageLog, int64, error) {
	r.calls++
	r.boundary = high
	r.after = after
	if after != nil {
		return []UsageLog{{ID: 1001}}, 2000, nil
	}
	out := make([]UsageLog, 1001)
	for i := range out {
		out[i] = UsageLog{ID: int64(i + 1), CreatedAt: time.Unix(100, 0), RequestedModel: " gpt-6.1-sol "}
	}
	return out, 2000, nil
}
func TestUsageExportCursorBindsOwnerFiltersAndBoundary(t *testing.T) {
	repo := &exportRepoFixture{}
	s := &UsageService{usageRepo: repo, exportCursorKey: []byte("fixture export signing key")}
	filters := usagestats.UsageLogFilters{UserID: 7, Model: "gpt-6.1-sol"}
	params := pagination.PaginationParams{SortBy: "created_at", SortOrder: "asc"}
	page, err := s.ExportPage(t.Context(), params, filters, "UTC", "")
	require.NoError(t, err)
	require.Len(t, page.Records, 1000)
	require.True(t, page.HasMore)
	cursor := page.NextCursor
	for range 2 {
		next, err := s.ExportPage(t.Context(), params, filters, "UTC", cursor)
		require.NoError(t, err)
		require.False(t, next.HasMore)
		require.Equal(t, int64(2000), repo.boundary)
		require.Equal(t, int64(1000), repo.after.ID)
	}
	calls := repo.calls
	for _, change := range []string{"owner", "model", "timezone", "sort", "signature", "huge", "expired"} {
		f, p, tz, c := filters, params, "UTC", cursor
		switch change {
		case "owner":
			f.UserID = 8
		case "model":
			f.Model = "another"
		case "timezone":
			tz = "Asia/Shanghai"
		case "sort":
			p.SortOrder = "desc"
		case "signature":
			c = strings.Repeat("A", len(cursor))
		case "huge":
			c = strings.Repeat("A", 8193)
		case "expired":
			parts := strings.Split(c, ".")
			raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
			var state usageExportCursor
			require.NoError(t, json.Unmarshal(raw, &state))
			state.Expires = time.Now().Add(-time.Second).Unix()
			raw, _ = json.Marshal(state)
			mac := hmac.New(sha256.New, s.exportCursorKey)
			_, _ = mac.Write(raw)
			c = base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		}
		_, err := s.ExportPage(t.Context(), p, f, tz, c)
		require.Error(t, err, change)
		require.Equal(t, calls, repo.calls, change)
	}
}
