package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestV027SeedanceTerminalEvidenceIsFirstWriterWins(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	original := `{"id":"task1","status":"succeeded","usage":{"completion_tokens":25}}`
	task := &service.SeedanceTask{ID: "local", State: "failed", Response: []byte(`{"id":"task1","status":"failed","usage":{"completion_tokens":0}}`)}
	query := `UPDATE seedance_tasks SET terminal_response=COALESCE(terminal_response,$2),state=COALESCE(terminal_response->>'status',$2::jsonb->>'status',state),updated_at=NOW() WHERE id=$1 RETURNING terminal_response,state`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("local", []byte(task.Response)).WillReturnRows(sqlmock.NewRows([]string{"terminal_response", "state"}).AddRow(original, "succeeded"))
	r := NewSeedanceTaskRepository(db)
	require.NoError(t, r.SaveTerminal(context.Background(), task))
	require.JSONEq(t, original, string(task.Response))
	require.Equal(t, "succeeded", task.State)
	mock.ExpectExec("UPDATE seedance_tasks SET .* WHERE id=\\$1 AND NOT settled AND terminal_response IS NULL").WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, r.Observe(context.Background(), "local", []byte(`{"status":"running"}`), "running", time.Second))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestV027SeedanceSettlementAtomicAndIdempotent(t *testing.T) {
	for _, scenario := range []string{"commit", "usage_error", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			r := NewSeedanceTaskRepository(db)
			task := &service.SeedanceTask{ID: "op", State: "succeeded", Response: []byte(`{"status":"succeeded","usage":{"completion_tokens":100}}`)}
			cmd := &service.UsageBillingCommand{RequestID: "op", APIKeyID: 2, UserID: 1, AccountID: 3, BalanceCost: 2, OutputTokens: 100}
			usage := &service.UsageLog{RequestID: "op", APIKeyID: 2, UserID: 1, AccountID: 3, ActualCost: 2, OutputTokens: 100}
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT settled FROM seedance_tasks").WithArgs("op").WillReturnRows(sqlmock.NewRows([]string{"settled"}).AddRow(scenario == "duplicate"))
			if scenario == "duplicate" {
				mock.ExpectRollback()
			} else {
				mock.ExpectQuery("INSERT INTO usage_billing_dedup").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
				mock.ExpectQuery("SELECT request_fingerprint FROM usage_billing_dedup_archive").WillReturnError(sql.ErrNoRows)
				mock.ExpectQuery("UPDATE users").WithArgs(2.0, int64(1)).WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(8.0))
				if scenario == "usage_error" {
					mock.ExpectQuery("INSERT INTO usage_logs").WillReturnError(errors.New("disk full"))
					mock.ExpectRollback()
				} else {
					mock.ExpectQuery("INSERT INTO usage_logs").WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(7, time.Now()))
					mock.ExpectExec("UPDATE seedance_tasks SET settled=TRUE").WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectCommit()
				}
			}
			result, err := r.Settle(context.Background(), task, cmd, usage)
			if scenario == "usage_error" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, scenario == "commit", result.Applied)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestV027SeedanceLookupScopesEveryOwnerDimension(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	group := int64(8)
	mock.ExpectQuery(`WHERE \(id=\$1 OR provider_id=\$1\) AND user_id=\$2 AND api_key_id=\$3 AND group_id IS NOT DISTINCT FROM \$4`).WithArgs("task", int64(1), int64(2), int64(8)).WillReturnError(sql.ErrNoRows)
	_, err = NewSeedanceTaskRepository(db).Owned(context.Background(), "task", 1, 2, &group)
	require.ErrorIs(t, err, service.ErrSeedanceTaskNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestV027SeedanceReservationBoundsInflightBeforeSubmission(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("seedance:user:1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("seedance:account:3").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT count").WithArgs(int64(1), int64(3)).WillReturnRows(sqlmock.NewRows([]string{"users", "accounts"}).AddRow(2, 2))
	mock.ExpectRollback()
	err = NewSeedanceTaskRepository(db).Reserve(context.Background(), &service.SeedanceTask{ID: "op", UserID: 1, AccountID: 3}, 2)
	require.ErrorIs(t, err, service.ErrSeedanceCapacity)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestV027SeedanceCollidingProviderIDsRequireLocalID(t *testing.T) {
	for _, localID := range []bool{false, true} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		id := "shared-provider-id"
		if localID {
			id = "local-1"
		}
		rows := sqlmock.NewRows([]string{"id", "provider_id", "user_id", "api_key_id", "account_id", "group_id", "state", "snapshot", "response", "created_at", "attempts", "settled", "effects_pending"})
		rows.AddRow("local-1", "shared-provider-id", 1, 2, 3, nil, "queued", `{}`, `{}`, time.Now(), 0, false, false)
		rows.AddRow("local-2", "shared-provider-id", 1, 2, 4, nil, "queued", `{}`, `{}`, time.Now(), 0, false, false)
		mock.ExpectQuery("SELECT .* FROM seedance_tasks").WithArgs(id, int64(1), int64(2), nil).WillReturnRows(rows)
		task, err := NewSeedanceTaskRepository(db).Owned(context.Background(), id, 1, 2, nil)
		if localID {
			require.NoError(t, err)
			require.Equal(t, "local-1", task.ID)
		} else {
			require.ErrorIs(t, err, service.ErrSeedanceAmbiguousID)
		}
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	}
}

func TestV027SeedanceCapabilitySurvivesSchedulerCredentialFiltering(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		credentials := map[string]any{"base_url": "https://provider.example", "api_key": "test-only", "seedance_enabled": enabled, "seedance_max_inflight": 7}
		filtered := filterSchedulerCredentials(credentials)
		account := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: filtered}
		require.Equal(t, enabled, account.SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilitySeedance))
		require.Equal(t, 7, filtered["seedance_max_inflight"])
	}
}
