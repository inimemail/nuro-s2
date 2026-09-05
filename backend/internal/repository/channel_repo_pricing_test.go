package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestChannelRepositoryUpdateModelPricingUsesIDAfterAllPricingFields(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &channelRepository{db: db}

	cacheWrite1hPrice := 0.000004
	pricing := &service.ChannelModelPricing{
		ID:                91,
		Models:            []string{"claude-sonnet-4"},
		BillingMode:       service.BillingModeToken,
		CacheWrite1hPrice: &cacheWrite1hPrice,
		Platform:          service.PlatformAnthropic,
		TimePricing: &service.ChannelTimePricing{
			Timezone: "UTC",
		},
	}

	mock.ExpectExec(`UPDATE channel_model_pricing[\s\S]+WHERE id = \$16`).
		WithArgs(
			sqlmock.AnyArg(),
			service.BillingModeToken,
			pricing.InputPrice,
			pricing.OutputPrice,
			pricing.CacheWritePrice,
			pricing.CacheWrite1hPrice,
			pricing.CacheReadPrice,
			pricing.FastMultiplier,
			pricing.FlexMultiplier,
			pricing.MaxReasoningEffortMultiplier,
			pricing.ImageInputPrice,
			pricing.ImageOutputPrice,
			pricing.PerRequestPrice,
			pricing.Platform,
			sqlmock.AnyArg(),
			pricing.ID,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, repo.UpdateModelPricing(context.Background(), pricing))
	require.NoError(t, mock.ExpectationsWereMet())
}
