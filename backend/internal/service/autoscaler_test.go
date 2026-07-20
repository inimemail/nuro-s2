package service

import (
	"context"
	"testing"
	"time"
)

type fakeCellProvider struct{ provisioned, ready string }

func (p *fakeCellProvider) Provision(_ context.Context, cellID string) (ProvisionedAdmissionCell, error) {
	p.provisioned = cellID
	return ProvisionedAdmissionCell{ID: cellID, Endpoint: "redis://cell:6379/0"}, nil
}

func (p *fakeCellProvider) WaitReady(_ context.Context, cell ProvisionedAdmissionCell) error {
	p.ready = cell.ID
	return nil
}

type fakeCellDirectory struct{ platform, registered, endpoint string }

func (d *fakeCellDirectory) RegisterCell(_ context.Context, platform, cellID, endpoint string) error {
	d.platform = platform
	d.registered = cellID
	d.endpoint = endpoint
	return nil
}

func TestRecommendPoolSizeUsesHysteresis(t *testing.T) {
	policy := AutoscalePoolPolicy{MinReplicas: 2, MaxReplicas: 100, TargetActive: 20000, TargetRPS: 3000}
	hold := RecommendPoolSize(policy, AutoscalePoolSample{CurrentReplicas: 4, Active: 70000, ObservedFor: 10 * time.Second})
	if hold.DesiredReplicas != 4 || hold.ScaleOut {
		t.Fatalf("short spike must hold: %+v", hold)
	}
	out := RecommendPoolSize(policy, AutoscalePoolSample{CurrentReplicas: 4, Active: 90000, ObservedFor: 30 * time.Second})
	if !out.ScaleOut || out.DesiredReplicas != 5 {
		t.Fatalf("sustained pressure must scale out: %+v", out)
	}
}

func TestAdmissionCellScaleOutRegistersDirectoryAndNeverAutoShrinks(t *testing.T) {
	out := RecommendAdmissionCells(AdmissionCellPolicy{}, AdmissionCellSample{
		Platform: PlatformOpenAI, CellCount: 2, LuaP99: 5 * time.Millisecond, ObservedFor: 30 * time.Second,
	})
	if out.AddCells != 1 || out.RequireMigration || !out.RequireDirectoryRegistration || out.AllowScaleIn {
		t.Fatalf("unexpected cell decision: %+v", out)
	}
	hold := RecommendAdmissionCells(AdmissionCellPolicy{}, AdmissionCellSample{Platform: PlatformOpenAI, CellCount: 4})
	if hold.AddCells != 0 || hold.AllowScaleIn {
		t.Fatalf("cell shrink must require an explicit migration: %+v", hold)
	}
}

func TestRecommendPoolSizeReservesWarmHeadroom(t *testing.T) {
	decision := RecommendPoolSize(AutoscalePoolPolicy{
		MinReplicas: 2, MaxReplicas: 20, TargetActive: 100, TargetRPS: 100,
		WarmHeadroom: 0.25,
	}, AutoscalePoolSample{CurrentReplicas: 2, Active: 160, ObservedFor: 30 * time.Second})
	if !decision.ScaleOut || decision.DesiredReplicas < 3 {
		t.Fatalf("warm headroom must be reserved: %+v", decision)
	}
}

func TestRecommendPoolSizeSuppressesScaleOutWhenCircuitOpen(t *testing.T) {
	decision := RecommendPoolSize(AutoscalePoolPolicy{MinReplicas: 2, MaxReplicas: 20}, AutoscalePoolSample{
		CurrentReplicas: 2, Active: 100000, QueueRatio: 1, CircuitOpen: true, ObservedFor: time.Minute,
	})
	if decision.ScaleOut || decision.Reason != "upstream_circuit_open_scale_suppressed" {
		t.Fatalf("circuit must suppress expansion: %+v", decision)
	}
}

func TestRecommendPoolSizeIgnoresUpstreamLatency(t *testing.T) {
	decision := RecommendPoolSize(AutoscalePoolPolicy{MinReplicas: 2, MaxReplicas: 20}, AutoscalePoolSample{
		CurrentReplicas: 2, UpstreamHeaderP99: 10 * time.Minute, ObservedFor: time.Minute,
	})
	if decision.ScaleOut {
		t.Fatalf("upstream latency alone must not scale the gateway: %+v", decision)
	}
}

func TestRecommendPoolSizeHonorsCostCeiling(t *testing.T) {
	decision := RecommendPoolSize(AutoscalePoolPolicy{
		MinReplicas: 2, MaxReplicas: 100, TargetActive: 100, TargetRPS: 100,
		MaxHourlyCost: 30, HourlyCostPerNode: 10,
	}, AutoscalePoolSample{CurrentReplicas: 2, Active: 10000, ObservedFor: time.Minute})
	if decision.DesiredReplicas > 3 {
		t.Fatalf("cost ceiling ignored: %+v", decision)
	}
}

func TestAdmissionCellControllerRegistersOnlyAfterReady(t *testing.T) {
	provider := &fakeCellProvider{}
	directory := &fakeCellDirectory{}
	controller := &AdmissionCellController{Provider: provider, Directory: directory}
	decision, err := controller.Reconcile(context.Background(), AdmissionCellPolicy{}, AdmissionCellSample{
		Platform: PlatformOpenAI, CellCount: 2, LuaP99: 5 * time.Millisecond, ObservedFor: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.RequireMigration || provider.provisioned != "openai-003" || provider.ready != "openai-003" || directory.platform != PlatformOpenAI || directory.registered != "openai-003" || directory.endpoint == "" {
		t.Fatalf("unsafe Cell reconcile: decision=%+v provider=%+v directory=%+v", decision, provider, directory)
	}
	second, err := controller.Reconcile(context.Background(), AdmissionCellPolicy{}, AdmissionCellSample{
		Platform: PlatformOpenAI, CellCount: 2, LuaP99: 5 * time.Millisecond, ObservedFor: time.Minute,
	})
	if err != nil || second.AddCells != 0 || second.Reason != "cell_already_registered" {
		t.Fatalf("stale reconcile provisioned duplicate Cell: %+v err=%v", second, err)
	}
}
