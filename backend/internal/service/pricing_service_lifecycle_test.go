package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type cancellablePricingRemote struct {
	started chan struct{}
	once    sync.Once
}

func (r *cancellablePricingRemote) FetchHashText(ctx context.Context, _ string) (string, error) {
	r.once.Do(func() { close(r.started) })
	<-ctx.Done()
	return "", ctx.Err()
}
func (r *cancellablePricingRemote) FetchPricingJSON(ctx context.Context, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestPricingLocalStartupDoesNotWaitForRemoteAndStopCancels(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model_pricing.json"), []byte(`{"test-model":{"input_cost_per_token":1}}`), 0600))
	cfg := &config.Config{Pricing: config.PricingConfig{DataDir: dir, RemoteURL: "https://prices.example/pricing.json", HashURL: "https://prices.example/pricing.sha256"}}
	remote := &cancellablePricingRemote{started: make(chan struct{})}
	svc := NewPricingService(cfg, remote)
	t.Cleanup(svc.Stop)
	initialized := make(chan error, 1)
	go func() { initialized <- svc.Initialize() }()
	select {
	case err := <-initialized:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("startup waited for remote hash")
	}
	require.Equal(t, float64(1), svc.GetModelPricing("test-model").InputCostPerToken)
	select {
	case <-remote.started:
	case <-time.After(3 * time.Second):
		t.Fatal("startup check never ran")
	}
	stopped := make(chan struct{})
	go func() { svc.Stop(); svc.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not cancel remote")
	}
	require.ErrorIs(t, svc.ForceUpdate(), context.Canceled)
	require.Equal(t, float64(1), svc.GetModelPricing("test-model").InputCostPerToken)
}

func TestPricingDisabledRemoteStartsNoNetworkWork(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model_pricing.json"), []byte(`{"test-model":{"input_cost_per_token":1}}`), 0600))
	svc := NewPricingService(&config.Config{Pricing: config.PricingConfig{DataDir: dir}}, nil)
	require.NoError(t, svc.Initialize())
	svc.Stop()
	svc.Stop()
	require.NotNil(t, svc.GetModelPricing("test-model"))
}

func TestPricingStoppedServiceDoesNotCopyFallbackOrReload(t *testing.T) {
	dir := t.TempDir()
	fallback := filepath.Join(dir, "fallback.json")
	require.NoError(t, os.WriteFile(fallback, []byte(`{"test-model":{"input_cost_per_token":1}}`), 0600))
	svc := NewPricingService(&config.Config{Pricing: config.PricingConfig{DataDir: dir, FallbackFile: fallback}}, nil)
	svc.Stop()
	require.ErrorIs(t, svc.useFallbackPricing(), context.Canceled)
	require.ErrorIs(t, svc.loadPricingData(fallback), context.Canceled)
	_, err := os.Stat(svc.getPricingFilePath())
	require.True(t, os.IsNotExist(err))
	require.Nil(t, svc.GetModelPricing("test-model"))
}
