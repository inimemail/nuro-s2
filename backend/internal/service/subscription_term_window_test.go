//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type termWindowActivationRepo struct {
	userSubRepoNoop
	windowStart time.Time
}

func (r *termWindowActivationRepo) ActivateWindows(_ context.Context, _ int64, start time.Time) error {
	r.windowStart = start
	return nil
}

type termWindowMonthlyResetRepo struct {
	userSubRepoNoop
	resetCalled bool
	resetAt     time.Time
}

func (r *termWindowMonthlyResetRepo) ResetMonthlyUsage(_ context.Context, _ int64, resetAt time.Time) error {
	r.resetCalled = true
	r.resetAt = resetAt
	return nil
}

func TestDelayedFirstUseAnchorsQuotaWindowAtActivation(t *testing.T) {
	repo := &termWindowActivationRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	startsAt := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	activatedAt := time.Date(2026, 7, 10, 23, 30, 0, 0, time.UTC)
	svc.now = func() time.Time { return activatedAt }
	sub := &UserSubscription{ID: 1, StartsAt: startsAt, ExpiresAt: startsAt.Add(45 * 24 * time.Hour)}

	require.NoError(t, svc.CheckAndActivateWindow(context.Background(), sub))
	require.Equal(t, activatedAt, repo.windowStart)

	resetAt, ok := sub.automaticWindowStartAt(&repo.windowStart, 30*24*time.Hour, activatedAt.Add(30*24*time.Hour))
	require.True(t, ok)
	require.Equal(t, activatedAt.Add(30*24*time.Hour), resetAt)
}

func TestThirtyDayTermDoesNotCreateMonthlyWindowAtExpiry(t *testing.T) {
	startsAt := time.Date(2026, 7, 1, 23, 30, 0, 0, time.UTC)
	expiresAt := startsAt.Add(30 * 24 * time.Hour)
	renewed := renewedSubscriptionTerm(&UserSubscription{}, "", startsAt, expiresAt)

	require.Equal(t, startsAt, *renewed.MonthlyWindowStart)
	require.True(t, renewed.NeedsMonthlyResetAt(expiresAt))
	require.False(t, renewed.canAutomaticallyResetMonthlyAt(expiresAt))
	require.Equal(t, expiresAt, *renewed.MonthlyResetTime())
}

func TestLegacyMonthlyAnchorUsesSubscriptionStartBoundary(t *testing.T) {
	legacyWindowStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	startsAt := time.Date(2026, 7, 1, 23, 30, 0, 0, time.UTC)
	now := startsAt.Add(30 * 24 * time.Hour)

	t.Run("exact term does not reset", func(t *testing.T) {
		repo := &termWindowMonthlyResetRepo{}
		svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
		svc.now = func() time.Time { return now }
		sub := &UserSubscription{
			ID: 1, StartsAt: startsAt, ExpiresAt: now,
			MonthlyWindowStart: &legacyWindowStart, MonthlyUsageUSD: 12,
		}

		require.NoError(t, svc.CheckAndResetWindows(context.Background(), sub))
		require.False(t, repo.resetCalled)
		require.Equal(t, 12.0, sub.MonthlyUsageUSD)
	})

	t.Run("partial final term resets", func(t *testing.T) {
		repo := &termWindowMonthlyResetRepo{}
		svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
		svc.now = func() time.Time { return now }
		sub := &UserSubscription{
			ID: 2, StartsAt: startsAt, ExpiresAt: startsAt.Add(45 * 24 * time.Hour),
			MonthlyWindowStart: &legacyWindowStart, MonthlyUsageUSD: 12,
		}

		require.NoError(t, svc.CheckAndResetWindows(context.Background(), sub))
		require.True(t, repo.resetCalled)
		require.Equal(t, startsAt.Add(30*24*time.Hour), repo.resetAt)
		require.Zero(t, sub.MonthlyUsageUSD)
	})
}

func TestAutomaticWindowPreservesManualAnchorAndAdvancesWholePeriods(t *testing.T) {
	startsAt := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	manualAnchor := startsAt.Add(5 * 24 * time.Hour)
	sub := &UserSubscription{StartsAt: startsAt, ExpiresAt: startsAt.Add(100 * 24 * time.Hour)}

	resetAt, ok := sub.automaticWindowStartAt(&manualAnchor, 30*24*time.Hour, manualAnchor.Add(65*24*time.Hour))
	require.True(t, ok)
	require.Equal(t, manualAnchor.Add(60*24*time.Hour), resetAt)
}

func TestValidateAndCheckLimitsRejectsExactSubscriptionExpiry(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	sub := &UserSubscription{Status: SubscriptionStatusActive, ExpiresAt: now}
	svc := NewSubscriptionService(groupRepoNoop{}, userSubRepoNoop{}, nil, nil, nil)
	svc.now = func() time.Time { return now }

	needsMaintenance, err := svc.ValidateAndCheckLimits(sub, &Group{})
	require.ErrorIs(t, err, ErrSubscriptionExpired)
	require.False(t, needsMaintenance)
}
