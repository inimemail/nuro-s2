package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

var openAIImageTaskTestColumns = []string{
	"id", "owner_id", "task_id", "api_key_id", "user_id", "user_concurrency",
	"endpoint", "model", "status", "request_body", "request_headers",
	"response_body", "status_code", "error_message", "locked_by", "locked_until",
	"attempts", "created_at", "updated_at", "started_at", "finished_at",
}

func openAIImageTaskTestRow(task *service.OpenAIImageTask, now time.Time) *sqlmock.Rows {
	return sqlmock.NewRows(openAIImageTaskTestColumns).AddRow(
		task.DBID, task.OwnerID, task.ID, task.APIKeyID, task.UserID, task.UserConcurrency,
		task.Endpoint, task.Model, service.OpenAIImageTaskStatusQueued, task.RequestBody,
		[]byte(`{"Content-Type":["application/json"]}`), nil, 0, "", "", nil,
		0, now, now, nil, nil,
	)
}

func newOpenAIImageTaskRepositoryTest(t *testing.T) (*openAIImageTaskRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock := newSQLMock(t)
	return &openAIImageTaskRepository{db: db, sql: db}, mock
}

func imageTaskSubmissionFixture() *service.OpenAIImageTask {
	return &service.OpenAIImageTask{
		ID: "task-1", OwnerID: "api_key:1", APIKeyID: 1, UserID: 10, UserConcurrency: 2,
		Endpoint: "/v1/images/generations", Model: "gpt-image-1", RequestBody: []byte(`{"prompt":"cat"}`),
		RequestHeaders: map[string][]string{"Content-Type": {"application/json"}},
	}
}

func expectOpenAIImageTaskSubmissionLock(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WithArgs(openAIImageTaskSubmitAdvisoryLockID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestOpenAIImageTaskSubmitWithinLimitReturnsExistingBeforeQueueLimit(t *testing.T) {
	repo, mock := newOpenAIImageTaskRepositoryTest(t)
	task := imageTaskSubmissionFixture()
	now := time.Now().UTC()
	expectOpenAIImageTaskSubmissionLock(mock)
	mock.ExpectQuery("SELECT id, owner_id, task_id").
		WithArgs(task.OwnerID, task.ID).
		WillReturnRows(openAIImageTaskTestRow(task, now))
	mock.ExpectCommit()

	stored, created, err := repo.SubmitWithinLimit(context.Background(), task, 1)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, task.ID, stored.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAIImageTaskSubmitWithinLimitRejectsQueueFullAtomically(t *testing.T) {
	repo, mock := newOpenAIImageTaskRepositoryTest(t)
	task := imageTaskSubmissionFixture()
	expectOpenAIImageTaskSubmissionLock(mock)
	mock.ExpectQuery("SELECT id, owner_id, task_id").
		WithArgs(task.OwnerID, task.ID).
		WillReturnRows(sqlmock.NewRows(openAIImageTaskTestColumns))
	mock.ExpectQuery("SELECT COUNT").
		WithArgs(service.OpenAIImageTaskStatusQueued, service.OpenAIImageTaskStatusRunning).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectRollback()

	stored, created, err := repo.SubmitWithinLimit(context.Background(), task, 1)
	require.Nil(t, stored)
	require.False(t, created)
	require.ErrorIs(t, err, service.ErrOpenAIImageTaskQueueFull)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAIImageTaskSubmitWithinLimitInsertsUnderLock(t *testing.T) {
	repo, mock := newOpenAIImageTaskRepositoryTest(t)
	task := imageTaskSubmissionFixture()
	task.DBID = 99
	now := time.Now().UTC()
	expectOpenAIImageTaskSubmissionLock(mock)
	mock.ExpectQuery("SELECT id, owner_id, task_id").
		WithArgs(task.OwnerID, task.ID).
		WillReturnRows(sqlmock.NewRows(openAIImageTaskTestColumns))
	mock.ExpectQuery("SELECT COUNT").
		WithArgs(service.OpenAIImageTaskStatusQueued, service.OpenAIImageTaskStatusRunning).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery("INSERT INTO openai_image_tasks").
		WithArgs(task.OwnerID, task.ID, task.APIKeyID, task.UserID, task.UserConcurrency, task.Endpoint, task.Model,
			service.OpenAIImageTaskStatusQueued, task.RequestBody, `{"Content-Type":["application/json"]}`).
		WillReturnRows(openAIImageTaskTestRow(task, now))
	mock.ExpectCommit()

	stored, created, err := repo.SubmitWithinLimit(context.Background(), task, 1)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, int64(99), stored.DBID)
	require.NoError(t, mock.ExpectationsWereMet())
}
