//go:build integration

package repository

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUsageExportPostgresKeysetBoundaryAndFilters(t *testing.T) {
	ctx := t.Context()
	client := testEntClient(t)
	suffix := uuid.NewString()
	user := mustCreateUser(t, client, &service.User{Email: suffix + "@example.com"})
	t.Cleanup(func() {
		ctx := context.Background()
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM usage_logs WHERE user_id=$1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE user_id=$1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id=$1", user.ID)
		require.NoError(t, err)
	})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-" + suffix})
	account := mustCreateAccount(t, client, &service.Account{Name: suffix})
	t.Cleanup(func() {
		// Usage rows must be removed before their account foreign key.
		_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM usage_logs WHERE user_id=$1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", account.ID)
		require.NoError(t, err)
	})
	repo := newUsageLogRepositoryWithSQL(client, integrationDB)
	created := time.Now().UTC().Truncate(time.Second)
	_, err := integrationDB.ExecContext(ctx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,requested_model,input_tokens,output_tokens,total_cost,actual_cost,created_at,stream,request_type,billing_type,billing_mode,upstream_model_mismatch)
	SELECT $1,$2,$3,$4||'/'||i,'base',CASE i%3 WHEN 0 THEN ' z-model ' WHEN 1 THEN 'a-model' ELSE '' END,10,20,0.5,0.5,$5::timestamptz+(i%3)*interval '1 second',i%2=0,CASE WHEN i%2=0 THEN 2 ELSE 1 END,0,'token',false FROM generate_series(1,1012) i`, user.ID, key.ID, account.ID, suffix, created)
	require.NoError(t, err)
	for _, sortBy := range []string{"id", "model", "created_at"} {
		for _, order := range []string{"asc", "desc"} {
			t.Run(sortBy+"/"+order, func(t *testing.T) {
				params := pagination.PaginationParams{SortBy: sortBy, SortOrder: order}
				filters := usagestats.UsageLogFilters{UserID: user.ID}
				logs, boundary, err := repo.ExportUsagePage(ctx, params, filters, 0, nil)
				require.NoError(t, err)
				require.Len(t, logs, 1001)
				expectedRows, err := integrationDB.QueryContext(ctx, "SELECT id FROM usage_logs WHERE user_id=$1 AND id<=$2 ORDER BY "+usageLogOrderBy(params), user.ID, boundary)
				require.NoError(t, err)
				var expected []int64
				for expectedRows.Next() {
					var id int64
					require.NoError(t, expectedRows.Scan(&id))
					expected = append(expected, id)
				}
				require.NoError(t, expectedRows.Err())
				require.NoError(t, expectedRows.Close())
				// New rows must not enter an already-started export, even when
				// their timestamp/model would place them between older rows.
				inserted := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, RequestID: uuid.NewString(), Model: "a-model", CreatedAt: created}
				_, err = repo.Create(ctx, inserted)
				require.NoError(t, err)
				var actual []int64
				for {
					more := len(logs) > 1000
					if more {
						logs = logs[:1000]
					}
					for _, log := range logs {
						actual = append(actual, log.ID)
					}
					if !more {
						break
					}
					last := logs[len(logs)-1]
					model := strings.Trim(last.RequestedModel, " ")
					if model == "" {
						model = last.Model
					}
					after := &service.UsageExportPosition{ID: last.ID, CreatedAt: last.CreatedAt, Model: model}
					var nextBoundary int64
					logs, nextBoundary, err = repo.ExportUsagePage(ctx, params, filters, boundary, after)
					require.NoError(t, err)
					require.Equal(t, boundary, nextBoundary)
				}
				require.Equal(t, expected, actual)
			})
		}
	}
	stream := true
	mismatch := false
	billing := int8(0)
	requestType := int16(2)
	end := created.Add(time.Minute)
	filters := usagestats.UsageLogFilters{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, Model: "base", Stream: &stream, RequestType: &requestType, BillingType: &billing, BillingMode: "token", UpstreamModelMismatch: &mismatch, StartTime: &created, EndTime: &end}
	params := pagination.PaginationParams{Page: 1, PageSize: 1000, SortBy: "created_at", SortOrder: "asc"}
	listed, _, err := repo.ListWithFilters(ctx, params, filters)
	require.NoError(t, err)
	exported, _, err := repo.ExportUsagePage(ctx, params, filters, 0, nil)
	require.NoError(t, err)
	listedIDs, exportedIDs := []int64{}, []int64{}
	for _, log := range listed {
		listedIDs = append(listedIDs, log.ID)
	}
	for _, log := range exported {
		exportedIDs = append(exportedIDs, log.ID)
	}
	require.NotEmpty(t, listedIDs)
	require.Equal(t, listedIDs, exportedIDs)
	filters.UserID = user.ID + 1000000
	exported, _, err = repo.ExportUsagePage(ctx, params, filters, 0, nil)
	require.NoError(t, err)
	require.Empty(t, exported)
	_, _, err = repo.ExportUsagePage(ctx, params, usagestats.UsageLogFilters{}, 0, nil)
	require.Error(t, err)
	t.Log(fmt.Sprintf("verified six stable sort directions and %d filtered rows", len(listedIDs)))
}
