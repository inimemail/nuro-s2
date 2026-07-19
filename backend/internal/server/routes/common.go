package routes

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/runtimeops"
	"github.com/gin-gonic/gin"
)

// RegisterCommonRoutes 注册通用路由（健康检查、状态等）
func RegisterCommonRoutes(r *gin.Engine, cfg *config.Config) {
	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/readyz", func(c *gin.Context) {
		if runtimeops.IsDraining() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "draining"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	r.GET("/metrics", func(c *gin.Context) {
		snapshot := runtimeops.Current()
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		body := fmt.Sprintf(
			"# TYPE sub2api_go_active_requests gauge\nsub2api_go_active_requests %d\n"+
				"# TYPE sub2api_go_requests_total counter\nsub2api_go_requests_total %d\n"+
				"# TYPE sub2api_go_server_errors_total counter\nsub2api_go_server_errors_total %d\n"+
				"# TYPE sub2api_go_drain_rejections_total counter\nsub2api_go_drain_rejections_total %d\n"+
				"# TYPE sub2api_go_request_duration_microseconds_total counter\nsub2api_go_request_duration_microseconds_total %d\n"+
				"# TYPE sub2api_go_draining gauge\nsub2api_go_draining %s\n"+
				"# TYPE sub2api_go_goroutines gauge\nsub2api_go_goroutines %d\n"+
				"# TYPE sub2api_go_heap_alloc_bytes gauge\nsub2api_go_heap_alloc_bytes %d\n"+
				"# TYPE sub2api_go_process_uptime_seconds gauge\nsub2api_go_process_uptime_seconds %.3f\n",
			snapshot.ActiveRequests,
			snapshot.TotalRequests,
			snapshot.ServerErrors,
			snapshot.RejectedDraining,
			snapshot.DurationMicros,
			runtimeops.BoolMetric(snapshot.Draining),
			runtime.NumGoroutine(),
			memory.HeapAlloc,
			time.Since(snapshot.StartedAt).Seconds(),
		)
		body += fmt.Sprintf("# TYPE sub2api_admission_claims_total counter\nsub2api_admission_claims_total %d\n", snapshot.AdmissionClaims)
		body += fmt.Sprintf("# TYPE sub2api_admission_claim_errors_total counter\nsub2api_admission_claim_errors_total %d\n", snapshot.AdmissionErrors)
		body += "# TYPE sub2api_admission_claim_duration_seconds histogram\n"
		labels := [...]string{"0.0005", "0.001", "0.002", "0.005", "0.01", "0.05", "0.1", "+Inf"}
		var cumulative uint64
		for i, count := range snapshot.AdmissionBuckets {
			cumulative += count
			body += fmt.Sprintf("sub2api_admission_claim_duration_seconds_bucket{le=\"%s\"} %d\n", labels[i], cumulative)
		}
		body += fmt.Sprintf("sub2api_admission_claim_duration_seconds_sum %.6f\n", float64(snapshot.AdmissionMicros)/1_000_000)
		body += fmt.Sprintf("sub2api_admission_claim_duration_seconds_count %d\n", snapshot.AdmissionClaims)
		body += fmt.Sprintf("# TYPE sub2api_go_heap_inuse_bytes gauge\nsub2api_go_heap_inuse_bytes %d\n", memory.HeapInuse)
		body += fmt.Sprintf("# TYPE sub2api_go_heap_sys_bytes gauge\nsub2api_go_heap_sys_bytes %d\n", memory.HeapSys)
		body += fmt.Sprintf("# TYPE sub2api_go_stack_inuse_bytes gauge\nsub2api_go_stack_inuse_bytes %d\n", memory.StackInuse)
		body += fmt.Sprintf("# TYPE sub2api_go_gc_cycles_total counter\nsub2api_go_gc_cycles_total %d\n", memory.NumGC)
		body += fmt.Sprintf("# TYPE sub2api_go_gc_pause_seconds_total counter\nsub2api_go_gc_pause_seconds_total %.9f\n", float64(memory.PauseTotalNs)/float64(time.Second))
		body += fmt.Sprintf("# TYPE sub2api_go_gc_cpu_fraction gauge\nsub2api_go_gc_cpu_fraction %.9f\n", memory.GCCPUFraction)
		c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(body))
	})
	r.POST("/internal/runtime/drain", func(c *gin.Context) {
		secret := ""
		if cfg != nil {
			secret = strings.TrimSpace(cfg.Gateway.OpenAIEdgeRS.InternalSecret)
		}
		provided := strings.TrimSpace(c.GetHeader("X-Sub2API-Edge-Secret"))
		if secret == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		runtimeops.SetDraining(true)
		c.JSON(http.StatusOK, gin.H{"status": "draining"})
	})

	// Claude Code 遥测日志（忽略，直接返回200）
	r.POST("/api/event_logging/batch", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Setup status endpoint (always returns needs_setup: false in normal mode)
	// This is used by the frontend to detect when the service has restarted after setup
	r.GET("/setup/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"needs_setup": false,
				"step":        "completed",
			},
		})
	})
}
