package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type userBatchLimitRepoStub struct {
	ids         []int64
	concurrency *int
	rpmLimit    *int
	affected    int
	err         error
}

type batchLimitAuthCacheInvalidatorStub struct{ userIDs []int64 }

func (*batchLimitAuthCacheInvalidatorStub) InvalidateAuthCacheByKey(context.Context, string) {}
func (s *batchLimitAuthCacheInvalidatorStub) InvalidateAuthCacheByUserID(_ context.Context, userID int64) {
	s.userIDs = append(s.userIDs, userID)
}
func (*batchLimitAuthCacheInvalidatorStub) InvalidateAuthCacheByGroupID(context.Context, int64) {}

func (r *userBatchLimitRepoStub) BatchUpdateLimits(_ context.Context, ids []int64, concurrency, rpmLimit *int) (int, error) {
	r.ids = append([]int64(nil), ids...)
	r.concurrency = concurrency
	r.rpmLimit = rpmLimit
	return r.affected, r.err
}

func TestAdminBatchUpdateLimitsPreservesZeroDeduplicatesAndInvalidates(t *testing.T) {
	zero, rpm := 0, 120
	repo := &userBatchLimitRepoStub{affected: 2}
	invalidator := &batchLimitAuthCacheInvalidatorStub{}
	svc := &adminServiceImpl{userLimitRepo: repo, authCacheInvalidator: invalidator}

	affected, err := svc.BatchUpdateLimits(context.Background(), []int64{7, 0, 7, -1, 9}, &zero, &rpm)
	require.NoError(t, err)
	require.Equal(t, 2, affected)
	require.Equal(t, []int64{7, 9}, repo.ids)
	require.NotNil(t, repo.concurrency)
	require.Zero(t, *repo.concurrency)
	require.Equal(t, 120, *repo.rpmLimit)
	require.Equal(t, []int64{7, 9}, invalidator.userIDs)
}

func TestAdminBatchUpdateLimitsDoesNotInvalidateAfterDatabaseFailure(t *testing.T) {
	wantErr := errors.New("write failed")
	value := 3
	repo := &userBatchLimitRepoStub{err: wantErr}
	invalidator := &batchLimitAuthCacheInvalidatorStub{}
	svc := &adminServiceImpl{userLimitRepo: repo, authCacheInvalidator: invalidator}

	_, err := svc.BatchUpdateLimits(context.Background(), []int64{7}, &value, nil)
	require.ErrorIs(t, err, wantErr)
	require.Empty(t, invalidator.userIDs)
}

func TestAdminBatchUpdateLimitsValidatesInputs(t *testing.T) {
	svc := &adminServiceImpl{userLimitRepo: &userBatchLimitRepoStub{}}
	_, err := svc.BatchUpdateLimits(context.Background(), []int64{1}, nil, nil)
	require.Error(t, err)

	negative := -1
	_, err = svc.BatchUpdateLimits(context.Background(), []int64{1}, &negative, nil)
	require.Error(t, err)

	_, err = svc.BatchUpdateLimits(context.Background(), []int64{0, -2}, new(int), nil)
	require.NoError(t, err)
}
