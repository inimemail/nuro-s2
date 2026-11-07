package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

type UsageExportPosition struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Model     string    `json:"model"`
}
type UsageExportRepository interface {
	ExportUsagePage(context.Context, pagination.PaginationParams, usagestats.UsageLogFilters, int64, *UsageExportPosition) ([]UsageLog, int64, error)
}
type usageExportCursor struct {
	Version   int                 `json:"v"`
	Scope     string              `json:"scope"`
	Expires   int64               `json:"expires"`
	HighWater int64               `json:"high_water"`
	After     UsageExportPosition `json:"after"`
}
type UsageExportPage struct {
	Records    []UsageLog
	NextCursor string
	HasMore    bool
}

func ProvideUsageService(repo UsageLogRepository, users UserRepository, client *dbent.Client, invalidator APIKeyAuthCacheInvalidator, cfg *config.Config) *UsageService {
	s := NewUsageService(repo, users, client, invalidator)
	if cfg != nil && len(cfg.JWT.Secret) >= 16 {
		mac := hmac.New(sha256.New, []byte(cfg.JWT.Secret))
		_, _ = mac.Write([]byte("nuro/usage-export/signing/v1"))
		s.exportCursorKey = mac.Sum(nil)
	}
	return s
}
func (s *UsageService) ExportPage(ctx context.Context, params pagination.PaginationParams, filters usagestats.UsageLogFilters, tz, cursor string) (*UsageExportPage, error) {
	bad := func() error {
		return infraerrors.BadRequest("INVALID_EXPORT_CURSOR", "Export cursor is invalid, expired or belongs to different filters")
	}
	if filters.UserID <= 0 {
		return nil, bad()
	}
	repo, ok := s.usageRepo.(UsageExportRepository)
	if !ok || len(s.exportCursorKey) == 0 {
		return nil, infraerrors.ServiceUnavailable("EXPORT_UNAVAILABLE", "Usage export is not available on this instance")
	}
	params.Page, params.PageSize = 1, 1000
	params.SortBy = strings.ToLower(strings.TrimSpace(params.SortBy))
	if params.SortBy != "model" && params.SortBy != "created_at" {
		params.SortBy = "id"
	}
	params.SortOrder = params.NormalizedSortOrder(pagination.SortOrderDesc)
	scopeBytes, err := json.Marshal(struct {
		Filters               usagestats.UsageLogFilters
		Sort, Order, Timezone string
	}{filters, params.SortBy, params.SortOrder, tz})
	if err != nil {
		return nil, err
	}
	scope := sha256.Sum256(scopeBytes)
	state := usageExportCursor{Version: 1, Scope: fmt.Sprintf("%x", scope), Expires: time.Now().Add(2 * time.Hour).Unix()}
	var after *UsageExportPosition
	if cursor != "" {
		if len(cursor) > 8192 {
			return nil, bad()
		}
		parts := strings.Split(cursor, ".")
		if len(parts) != 2 {
			return nil, bad()
		}
		payload, e1 := base64.RawURLEncoding.DecodeString(parts[0])
		sig, e2 := base64.RawURLEncoding.DecodeString(parts[1])
		mac := hmac.New(sha256.New, s.exportCursorKey)
		_, _ = mac.Write(payload)
		if e1 != nil || e2 != nil || !hmac.Equal(sig, mac.Sum(nil)) {
			return nil, bad()
		}
		expected := state.Scope
		if json.Unmarshal(payload, &state) != nil || state.Version != 1 || state.Scope != expected || state.Expires <= time.Now().Unix() || state.HighWater <= 0 || state.After.ID <= 0 || state.After.ID > state.HighWater {
			return nil, bad()
		}
		after = &state.After
	}
	logs, highWater, err := repo.ExportUsagePage(ctx, params, filters, state.HighWater, after)
	if err != nil {
		return nil, err
	}
	page := &UsageExportPage{Records: logs, HasMore: len(logs) > 1000}
	if page.HasMore {
		page.Records = logs[:1000]
		last := page.Records[len(page.Records)-1]
		model := strings.Trim(last.RequestedModel, " ")
		if model == "" {
			model = last.Model
		}
		state.HighWater = highWater
		state.After = UsageExportPosition{ID: last.ID, CreatedAt: last.CreatedAt, Model: model}
		payload, err := json.Marshal(state)
		if err != nil {
			return nil, err
		}
		mac := hmac.New(sha256.New, s.exportCursorKey)
		_, _ = mac.Write(payload)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	return page, nil
}
