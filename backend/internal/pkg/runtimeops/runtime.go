package runtimeops

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

var process = newProcessState()

type processState struct {
	startedAt        time.Time
	draining         atomic.Bool
	activeRequests   atomic.Int64
	totalRequests    atomic.Uint64
	rejectedDraining atomic.Uint64
	serverErrors     atomic.Uint64
	durationMicros   atomic.Uint64
	admissionClaims  atomic.Uint64
	admissionErrors  atomic.Uint64
	admissionMicros  atomic.Uint64
	admissionBuckets [8]atomic.Uint64
}

func newProcessState() *processState {
	return &processState{startedAt: time.Now()}
}

type Snapshot struct {
	StartedAt        time.Time
	Draining         bool
	ActiveRequests   int64
	TotalRequests    uint64
	RejectedDraining uint64
	ServerErrors     uint64
	DurationMicros   uint64
	AdmissionClaims  uint64
	AdmissionErrors  uint64
	AdmissionMicros  uint64
	AdmissionBuckets [8]uint64
}

func Current() Snapshot {
	snapshot := Snapshot{
		StartedAt:        process.startedAt,
		Draining:         process.draining.Load(),
		ActiveRequests:   process.activeRequests.Load(),
		TotalRequests:    process.totalRequests.Load(),
		RejectedDraining: process.rejectedDraining.Load(),
		ServerErrors:     process.serverErrors.Load(),
		DurationMicros:   process.durationMicros.Load(),
		AdmissionClaims:  process.admissionClaims.Load(),
		AdmissionErrors:  process.admissionErrors.Load(),
		AdmissionMicros:  process.admissionMicros.Load(),
	}
	for i := range snapshot.AdmissionBuckets {
		snapshot.AdmissionBuckets[i] = process.admissionBuckets[i].Load()
	}
	return snapshot
}

func ObserveAdmissionClaim(duration time.Duration, err error) {
	process.admissionClaims.Add(1)
	process.admissionMicros.Add(uint64(max(duration.Microseconds(), 0)))
	if err != nil {
		process.admissionErrors.Add(1)
	}
	ms := float64(duration) / float64(time.Millisecond)
	limits := [...]float64{0.5, 1, 2, 5, 10, 50, 100}
	for i, limit := range limits {
		if ms <= limit {
			process.admissionBuckets[i].Add(1)
			return
		}
	}
	process.admissionBuckets[7].Add(1)
}

func SetDraining(value bool) {
	process.draining.Store(value)
}

func IsDraining() bool {
	return process.draining.Load()
}

func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := ""
		if c.Request != nil && c.Request.URL != nil {
			path = c.Request.URL.Path
		}
		if process.draining.Load() && !allowedWhileDraining(path) {
			process.rejectedDraining.Add(1)
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(503, gin.H{"error": "service draining"})
			return
		}

		process.activeRequests.Add(1)
		process.totalRequests.Add(1)
		started := time.Now()
		defer func() {
			process.durationMicros.Add(uint64(time.Since(started).Microseconds()))
			process.activeRequests.Add(-1)
			if c.Writer.Status() >= 500 {
				process.serverErrors.Add(1)
			}
		}()
		c.Next()
	}
}

func allowedWhileDraining(path string) bool {
	return path == "/health" || path == "/readyz" || path == "/metrics" ||
		strings.HasPrefix(path, "/internal/")
}

func BoolMetric(value bool) string {
	return strconv.FormatInt(int64(boolToInt(value)), 10)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
