package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"time"
)

// These management-only ports keep ordinary admission on its existing reads.
// Redis fillers use a generation fetched with the original database object.
type GeminiCacheBalanceWriter interface {
	SetUserBalanceWithGeneration(context.Context, int64, float64, int64) error
}

type GeminiCacheAdmissionPublisher interface {
	PublishCacheChargeAdmission(context.Context, *GeminiCacheOperation) error
}

func geminiCachePlatformSettled(d, w, m float64, ds, ws, ms *time.Time, now time.Time) (float64, float64, float64) {
	if quotaWindowExpired(ds, timezone.StartOfDay(now)) {
		d = 0
	}
	if quotaWindowExpired(ws, timezone.StartOfWeek(now)) {
		w = 0
	}
	if monthlyQuotaWindowExpired(ms, now) {
		m = 0
	}
	return d, w, m
}
