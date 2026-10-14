package service

import (
	"time"
)

// SubscriptionCacheData represents cached subscription data
type SubscriptionCacheData struct {
	DailyWindowStart      *time.Time
	WeeklyWindowStart     *time.Time
	MonthlyWindowStart    *time.Time
	GeminiCachePending    float64
	GeminiCacheGeneration int64
	Status                string
	ExpiresAt             time.Time
	DailyUsage            float64
	WeeklyUsage           float64
	MonthlyUsage          float64
	Version               int64
}
