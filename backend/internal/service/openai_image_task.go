package service

import (
	"context"
	"errors"
	"time"
)

const (
	OpenAIImageTaskStatusQueued  = "queued"
	OpenAIImageTaskStatusRunning = "running"
	OpenAIImageTaskStatusSuccess = "success"
	OpenAIImageTaskStatusError   = "error"
)

var ErrOpenAIImageTaskQueueFull = errors.New("image task queue is full")

type OpenAIImageTask struct {
	DBID            int64
	ID              string
	OwnerID         string
	APIKeyID        int64
	UserID          int64
	UserConcurrency int
	Status          string
	Endpoint        string
	Model           string
	RequestBody     []byte
	RequestHeaders  map[string][]string
	StatusCode      int
	Response        []byte
	ErrorMessage    string
	LockedBy        string
	LockedUntil     *time.Time
	Attempts        int
	CreatedAt       time.Time
	UpdatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

type OpenAIImageTaskRepository interface {
	SubmitWithinLimit(ctx context.Context, task *OpenAIImageTask, maxUnfinished int) (*OpenAIImageTask, bool, error)
	List(ctx context.Context, ownerID string, ids []string, limit int) ([]*OpenAIImageTask, []string, error)
	ClaimNext(ctx context.Context, workerID string, lockFor time.Duration) (*OpenAIImageTask, error)
	MarkSuccess(ctx context.Context, dbID int64, statusCode int, response []byte) error
	MarkError(ctx context.Context, dbID int64, statusCode int, message string, response []byte) error
	CleanupFinished(ctx context.Context, olderThan time.Time, limit int) (int64, error)
}
