package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

type AutoscalePoolPolicy struct {
	MinReplicas       int
	MaxReplicas       int
	TargetActive      float64
	TargetRPS         float64
	MaxHourlyCost     float64
	HourlyCostPerNode float64
	// WarmHeadroom is the fraction of each pool that must remain available
	// for bursts while new replicas warm up. It is deliberately accounted for
	// in desired capacity, rather than by changing request admission.
	WarmHeadroom      float64
	ScaleOutThreshold float64
	ScaleInThreshold  float64
	ScaleOutFor       time.Duration
	ScaleInFor        time.Duration
}

type AutoscalePoolSample struct {
	CurrentReplicas int
	Active          float64
	RPS             float64
	QueueRatio      float64
	// SchedulerP99 and RedisClaimP99 are controllable gateway stages. A slow
	// upstream must never cause an unbounded gateway scale-out.
	SchedulerP99      time.Duration
	RedisClaimP99     time.Duration
	UpstreamHeaderP99 time.Duration
	FDUsageRatio      float64
	CPUUsageRatio     float64
	GCPressureRatio   float64
	CircuitOpen       bool
	ObservedFor       time.Duration
}

type AutoscalePoolDecision struct {
	DesiredReplicas int
	ScaleOut        bool
	ScaleIn         bool
	Reason          string
}

// AutoscaleDecisionAudit is the stable, privacy-free record emitted with each
// decision. It is intentionally made of control-plane measurements only.
type AutoscaleDecisionAudit struct {
	At              time.Time
	CurrentReplicas int
	DesiredReplicas int
	Reason          string
	CircuitOpen     bool
	WarmHeadroom    float64
}

func NewAutoscaleDecisionAudit(policy AutoscalePoolPolicy, sample AutoscalePoolSample, decision AutoscalePoolDecision, at time.Time) AutoscaleDecisionAudit {
	policy = normalizedAutoscalePolicy(policy)
	return AutoscaleDecisionAudit{
		At:              at,
		CurrentReplicas: sample.CurrentReplicas,
		DesiredReplicas: decision.DesiredReplicas,
		Reason:          decision.Reason,
		CircuitOpen:     sample.CircuitOpen,
		WarmHeadroom:    policy.WarmHeadroom,
	}
}

func RecommendPoolSize(policy AutoscalePoolPolicy, sample AutoscalePoolSample) AutoscalePoolDecision {
	policy = normalizedAutoscalePolicy(policy)
	current := autoscaleClampInt(sample.CurrentReplicas, policy.MinReplicas, policy.MaxReplicas)
	effectiveActiveTarget := policy.TargetActive * (1 - policy.WarmHeadroom)
	effectiveRPSTarget := policy.TargetRPS * (1 - policy.WarmHeadroom)
	byActive := int(math.Ceil(sample.Active / effectiveActiveTarget))
	byRPS := int(math.Ceil(sample.RPS / effectiveRPSTarget))
	desired := autoscaleClampInt(autoscaleMaxInt(policy.MinReplicas, byActive, byRPS), policy.MinReplicas, policy.MaxReplicas)
	costCeiling := policy.MaxReplicas
	if policy.MaxHourlyCost > 0 && policy.HourlyCostPerNode > 0 {
		costLimited := int(math.Floor(policy.MaxHourlyCost / policy.HourlyCostPerNode))
		if costLimited < policy.MinReplicas {
			costLimited = policy.MinReplicas
		}
		costCeiling = autoscaleMinInt(policy.MaxReplicas, costLimited)
		desired = autoscaleClampInt(desired, policy.MinReplicas, costCeiling)
	}
	pressure := math.Max(math.Max(sample.QueueRatio, sample.FDUsageRatio), math.Max(sample.CPUUsageRatio, sample.GCPressureRatio))
	pressure = math.Max(pressure, math.Max(
		safeRatio(sample.Active, float64(current)*effectiveActiveTarget),
		safeRatio(sample.RPS, float64(current)*effectiveRPSTarget),
	))
	// Stage latency thresholds are intentionally conservative. They are only
	// used as local pressure signals; upstream_header_ms is observability, not
	// an autoscaling trigger.
	if sample.SchedulerP99 > 0 {
		pressure = math.Max(pressure, safeRatio(float64(sample.SchedulerP99), float64(100*time.Millisecond)))
	}
	if sample.RedisClaimP99 > 0 {
		pressure = math.Max(pressure, safeRatio(float64(sample.RedisClaimP99), float64(10*time.Millisecond)))
	}

	if !sample.CircuitOpen && pressure >= policy.ScaleOutThreshold && sample.ObservedFor >= policy.ScaleOutFor {
		if current >= costCeiling {
			return AutoscalePoolDecision{DesiredReplicas: current, Reason: "cost_ceiling_load_shed_required"}
		}
		if desired <= current {
			desired = current + 1
		}
		desired = autoscaleClampInt(desired, policy.MinReplicas, policy.MaxReplicas)
		return AutoscalePoolDecision{DesiredReplicas: desired, ScaleOut: desired > current, Reason: "sustained_controllable_capacity_pressure"}
	}
	if sample.CircuitOpen && pressure >= policy.ScaleOutThreshold {
		return AutoscalePoolDecision{DesiredReplicas: current, Reason: "upstream_circuit_open_scale_suppressed"}
	}
	if pressure <= policy.ScaleInThreshold && sample.ObservedFor >= policy.ScaleInFor && desired < current {
		return AutoscalePoolDecision{DesiredReplicas: desired, ScaleIn: true, Reason: "sustained_low_utilization"}
	}
	return AutoscalePoolDecision{DesiredReplicas: current, Reason: "hysteresis_hold"}
}

type AdmissionCellPolicy struct {
	MaxLuaP99             time.Duration
	MaxCommandUtilization float64
	MaxMemoryUtilization  float64
	ScaleOutFor           time.Duration
}

type AdmissionCellSample struct {
	Platform           string
	CellCount          int
	LuaP99             time.Duration
	CommandUtilization float64
	MemoryUtilization  float64
	HotKeySkew         float64
	ObservedFor        time.Duration
	MigrationActive    bool
}

type AdmissionCellDecision struct {
	AddCells                     int
	AllowScaleIn                 bool
	RequireMigration             bool
	RequireDirectoryRegistration bool
	Reason                       string
}

// AdmissionCellProvider is the only infrastructure-specific boundary. A Redis
// Operator, AWS/GCP/Azure service, or an internal provisioning API implements
// it without leaking provider logic into admission or scheduling.
type AdmissionCellProvider interface {
	Provision(ctx context.Context, cellID string) error
	WaitReady(ctx context.Context, cellID string) error
}

type AdmissionCellDirectoryRegistrar interface {
	RegisterPlatformForNewAccounts(ctx context.Context, platform, cellID string) error
}

type AdmissionCellAuditSink interface {
	RecordAdmissionCellDecision(ctx context.Context, decision AdmissionCellDecision, cellID string, err error)
}

type AdmissionCellController struct {
	Provider   AdmissionCellProvider
	Directory  AdmissionCellDirectoryRegistrar
	Audit      AdmissionCellAuditSink
	mu         sync.Mutex
	registered map[string]struct{}
}

// Reconcile provisions at most one Cell per decision. Existing ownership is
// untouched; successful registration affects new accounts only.
func (c *AdmissionCellController) Reconcile(ctx context.Context, policy AdmissionCellPolicy, sample AdmissionCellSample) (AdmissionCellDecision, error) {
	decision := RecommendAdmissionCells(policy, sample)
	if decision.AddCells == 0 {
		if c != nil && c.Audit != nil {
			c.Audit.RecordAdmissionCellDecision(ctx, decision, "", nil)
		}
		return decision, nil
	}
	if c == nil || c.Provider == nil || c.Directory == nil {
		return decision, errors.New("admission Cell provider and directory registrar are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	platform := normalizeAdmissionCellPlatform(sample.Platform)
	if platform == "" {
		return decision, fmt.Errorf("unsupported admission Cell platform %q", sample.Platform)
	}
	cellID := fmt.Sprintf("%s-%03d", platform, sample.CellCount+1)
	if _, exists := c.registered[cellID]; exists {
		decision.AddCells = 0
		decision.RequireDirectoryRegistration = false
		decision.Reason = "cell_already_registered"
		return decision, nil
	}
	var err error
	if err = c.Provider.Provision(ctx, cellID); err == nil {
		err = c.Provider.WaitReady(ctx, cellID)
	}
	if err == nil {
		err = c.Directory.RegisterPlatformForNewAccounts(ctx, platform, cellID)
	}
	if c.Audit != nil {
		c.Audit.RecordAdmissionCellDecision(ctx, decision, cellID, err)
	}
	if err != nil {
		return decision, fmt.Errorf("provision admission Cell %s: %w", cellID, err)
	}
	if c.registered == nil {
		c.registered = make(map[string]struct{})
	}
	c.registered[cellID] = struct{}{}
	return decision, nil
}

func RecommendAdmissionCells(policy AdmissionCellPolicy, sample AdmissionCellSample) AdmissionCellDecision {
	if normalizeAdmissionCellPlatform(sample.Platform) == "" {
		return AdmissionCellDecision{Reason: "invalid_platform"}
	}
	if policy.MaxLuaP99 <= 0 {
		policy.MaxLuaP99 = 2 * time.Millisecond
	}
	if policy.MaxCommandUtilization <= 0 {
		policy.MaxCommandUtilization = 0.70
	}
	if policy.MaxMemoryUtilization <= 0 {
		policy.MaxMemoryUtilization = 0.65
	}
	if policy.ScaleOutFor <= 0 {
		policy.ScaleOutFor = 30 * time.Second
	}
	if sample.MigrationActive {
		return AdmissionCellDecision{Reason: "migration_in_progress"}
	}
	overloaded := sample.LuaP99 > policy.MaxLuaP99 ||
		sample.CommandUtilization > policy.MaxCommandUtilization ||
		sample.MemoryUtilization > policy.MaxMemoryUtilization || sample.HotKeySkew > 2
	if overloaded && sample.ObservedFor >= policy.ScaleOutFor {
		// A new Cell is registered only for new account assignments. Existing
		// account owners remain frozen, so normal scale-out does not migrate any
		// live slot state.
		return AdmissionCellDecision{AddCells: 1, RequireDirectoryRegistration: true, Reason: "cell_capacity_pressure"}
	}
	// Removing a Cell is never automatic: it requires proving every lease/key
	// has migrated and that no Lua transaction can still address the old Cell.
	return AdmissionCellDecision{AllowScaleIn: false, Reason: "safe_hold"}
}

func normalizeAdmissionCellPlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case PlatformOpenAI:
		return PlatformOpenAI
	case PlatformAnthropic:
		return PlatformAnthropic
	default:
		return ""
	}
}

func normalizedAutoscalePolicy(policy AutoscalePoolPolicy) AutoscalePoolPolicy {
	if policy.MinReplicas <= 0 {
		policy.MinReplicas = 2
	}
	if policy.MaxReplicas < policy.MinReplicas {
		policy.MaxReplicas = policy.MinReplicas
	}
	if policy.TargetActive <= 0 {
		policy.TargetActive = 20000
	}
	if policy.TargetRPS <= 0 {
		policy.TargetRPS = 3000
	}
	if math.IsNaN(policy.WarmHeadroom) || math.IsInf(policy.WarmHeadroom, 0) || policy.WarmHeadroom < 0 {
		policy.WarmHeadroom = 0
	}
	if policy.WarmHeadroom >= 1 {
		policy.WarmHeadroom = 0.20
	}
	if policy.ScaleOutThreshold <= 0 {
		policy.ScaleOutThreshold = 0.70
	}
	if policy.ScaleInThreshold <= 0 {
		policy.ScaleInThreshold = 0.25
	}
	if policy.ScaleOutFor <= 0 {
		policy.ScaleOutFor = 30 * time.Second
	}
	if policy.ScaleInFor <= 0 {
		policy.ScaleInFor = 10 * time.Minute
	}
	return policy
}

func safeRatio(value, capacity float64) float64 {
	if capacity <= 0 {
		return 0
	}
	return value / capacity
}

func autoscaleClampInt(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func autoscaleMaxInt(values ...int) int {
	result := 0
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}

func autoscaleMinInt(values ...int) int {
	if len(values) == 0 {
		return 0
	}
	result := values[0]
	for _, value := range values[1:] {
		if value < result {
			result = value
		}
	}
	return result
}
