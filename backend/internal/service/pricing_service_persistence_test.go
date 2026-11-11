package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type failingPricingFile struct {
	*os.File
	stage string
}

func (f failingPricingFile) Write(b []byte) (int, error) {
	if f.stage == "write" {
		return 0, errors.New("write failed")
	}
	if f.stage == "short" {
		return len(b) - 1, nil
	}
	return f.File.Write(b)
}
func (f failingPricingFile) Sync() error {
	if f.stage == "sync" {
		return errors.New("sync failed")
	}
	return f.File.Sync()
}
func (f failingPricingFile) Close() error {
	err := f.File.Close()
	if f.stage == "close" {
		return errors.New("close failed")
	}
	return err
}

func TestPricingAtomicReplacementFailuresKeepOldFile(t *testing.T) {
	for _, stage := range []string{"create", "write", "short", "sync", "close", "rename"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "price.json")
			require.NoError(t, os.WriteFile(path, []byte(`{"old":1}`), 0600))
			ops := pricingFileOps{
				stat: os.Stat, remove: os.Remove,
				createTemp: func(dir, pattern string) (pricingTempFile, error) {
					if stage == "create" {
						return nil, errors.New("create failed")
					}
					f, err := os.CreateTemp(dir, pattern)
					return failingPricingFile{f, stage}, err
				},
				rename: func(old, next string) error {
					if stage == "rename" {
						return errors.New("rename failed")
					}
					return os.Rename(old, next)
				},
			}
			err := writeAtomicPricingFileWithOps(path, []byte(`{"new":2}`), ops)
			require.Error(t, err)
			if stage == "short" {
				require.ErrorIs(t, err, io.ErrShortWrite)
			}
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, `{"old":1}`, string(body))
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Len(t, entries, 1, "temporary files must be cleaned")
		})
	}
}

func TestPricingAtomicReplacementPreservesPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "price.json")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0600))
	require.NoError(t, writeAtomicPricingFile(path, []byte("new")))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

type pricingRemoteFixture struct {
	mu      sync.Mutex
	bodies  [][]byte
	hash    string
	started chan struct{}
	proceed chan struct{}
}

func (f *pricingRemoteFixture) FetchHashText(context.Context, string) (string, error) {
	return f.hash, nil
}
func (f *pricingRemoteFixture) FetchPricingJSON(ctx context.Context, _ string) ([]byte, error) {
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-f.proceed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body := f.bodies[0]
	if len(f.bodies) > 1 {
		f.bodies = f.bodies[1:]
	}
	return body, nil
}

func pricingSyncFixture(t *testing.T, remote *pricingRemoteFixture) *PricingService {
	t.Helper()
	return &PricingService{cfg: &config.Config{Pricing: config.PricingConfig{
		DataDir: t.TempDir(), RemoteURL: "https://prices.example/pricing.json", HashURL: "https://prices.example/pricing.sha256",
	}}, remoteClient: remote, pricingData: map[string]*LiteLLMModelPricing{"test-model": {InputCostPerToken: 1}}}
}

func TestPricingHashMismatchRetriesAndRestartUsesBody(t *testing.T) {
	body1 := []byte(`{"test-model":{"input_cost_per_token":2}}`)
	body2 := []byte(`{"test-model":{"input_cost_per_token":3}}`)
	svc := pricingSyncFixture(t, &pricingRemoteFixture{hash: strings.Repeat("a", 64), bodies: [][]byte{body1, body2}})
	require.NoError(t, svc.downloadPricingData())
	digest := sha256.Sum256(body1)
	require.Equal(t, hex.EncodeToString(digest[:]), svc.localHash)
	require.NoError(t, svc.syncWithRemote())
	require.Equal(t, float64(3), svc.GetModelPricing("test-model").InputCostPerToken)
	// Simulate a restart after body rename and before hash rename.
	require.NoError(t, os.WriteFile(svc.getHashFilePath(), []byte(strings.Repeat("b", 64)), 0600))
	restarted := &PricingService{cfg: svc.cfg}
	require.NoError(t, restarted.loadPricingData(svc.getPricingFilePath()))
	digest = sha256.Sum256(body2)
	require.Equal(t, hex.EncodeToString(digest[:]), restarted.localHash)
}

func TestPricingPersistenceFailureIsRetried(t *testing.T) {
	body := []byte(`{"test-model":{"input_cost_per_token":2}}`)
	svc := pricingSyncFixture(t, &pricingRemoteFixture{bodies: [][]byte{body}})
	// A directory at the sidecar path makes the second rename fail deterministically.
	require.NoError(t, os.Mkdir(svc.getHashFilePath(), 0700))
	require.NoError(t, svc.downloadPricingData())
	require.Equal(t, float64(2), svc.GetModelPricing("test-model").InputCostPerToken)
	require.Equal(t, body, svc.pendingPricingBody)
	require.NoError(t, os.Remove(svc.getHashFilePath()))
	svc.updateMu.Lock()
	err := svc.retryPricingPersistenceLocked()
	svc.updateMu.Unlock()
	require.NoError(t, err)
	require.Nil(t, svc.pendingPricingBody)
	onDisk, err := os.ReadFile(svc.getPricingFilePath())
	require.NoError(t, err)
	require.Equal(t, body, onDisk)
}

func TestPricingSlowUpdateDoesNotBlockReadsAndSerializesWriters(t *testing.T) {
	remote := &pricingRemoteFixture{started: make(chan struct{}), proceed: make(chan struct{}), bodies: [][]byte{
		[]byte(`{"test-model":{"input_cost_per_token":2}}`), []byte(`{"test-model":{"input_cost_per_token":3}}`),
	}}
	svc := pricingSyncFixture(t, remote)
	first := make(chan error, 1)
	go func() { first <- svc.downloadPricingData() }()
	<-remote.started
	read := make(chan float64, 1)
	go func() { read <- svc.GetModelPricing("test-model").InputCostPerToken }()
	select {
	case value := <-read:
		require.Equal(t, float64(1), value)
	case <-time.After(time.Second):
		t.Fatal("price read blocked by remote update")
	}
	second := make(chan error, 1)
	go func() { second <- svc.downloadPricingData() }()
	remote.proceed <- struct{}{}
	require.NoError(t, <-first)
	<-remote.started
	remote.proceed <- struct{}{}
	require.NoError(t, <-second)
	require.Equal(t, float64(3), svc.GetModelPricing("test-model").InputCostPerToken)
}

func TestPricingInvalidOverlayKeepsLastGoodSnapshot(t *testing.T) {
	for _, invalid := range []string{`null`, `[]`, `{"test-model":{"input_cost_per_token":"free"}}`, `{"test-model":{"input_cost_per_token":-1}}`} {
		t.Run(invalid, func(t *testing.T) {
			body := []byte(`{"test-model":{"input_cost_per_token":2}}`)
			svc := pricingSyncFixture(t, &pricingRemoteFixture{bodies: [][]byte{body, body}})
			overlay := filepath.Join(svc.cfg.Pricing.DataDir, "override.json")
			svc.cfg.Pricing.OverrideFile = overlay
			require.NoError(t, os.WriteFile(overlay, []byte(`{"test-model":{"input_cost_per_token":0}}`), 0600))
			require.NoError(t, svc.downloadPricingData())
			require.Zero(t, svc.GetModelPricing("test-model").InputCostPerToken, "explicit zero is a valid overlay")
			require.NoError(t, os.WriteFile(overlay, []byte(invalid), 0600))
			require.Error(t, svc.downloadPricingData())
			require.Zero(t, svc.GetModelPricing("test-model").InputCostPerToken, "invalid replacement must not silently remove the valid override")
			require.Error(t, svc.loadPricingData(svc.getPricingFilePath()))
			require.Zero(t, svc.GetModelPricing("test-model").InputCostPerToken)
		})
	}
}

func TestPricingInvalidFallbackKeepsLastGoodSnapshot(t *testing.T) {
	for _, invalid := range []string{`{"fallback-only":{"input_cost_per_token":-1}}`, `{"fallback-only":{"input_cost_per_token":"free"}}`, `{"fallback-only":null}`} {
		t.Run(invalid, func(t *testing.T) {
			body := []byte(`{"test-model":{"input_cost_per_token":2}}`)
			svc := pricingSyncFixture(t, &pricingRemoteFixture{bodies: [][]byte{body, body}})
			fallback := filepath.Join(svc.cfg.Pricing.DataDir, "fallback.json")
			svc.cfg.Pricing.FallbackFile = fallback
			require.NoError(t, os.WriteFile(fallback, []byte(`{"fallback-only":{"input_cost_per_token":5,"search_context_cost_per_query":{"high":0.01}}}`), 0600))
			require.NoError(t, svc.downloadPricingData())
			require.Equal(t, float64(5), svc.GetModelPricing("fallback-only").InputCostPerToken)
			require.NoError(t, os.WriteFile(fallback, []byte(invalid), 0600))
			require.Error(t, svc.downloadPricingData())
			require.Equal(t, float64(5), svc.GetModelPricing("fallback-only").InputCostPerToken)
			require.Error(t, svc.loadPricingData(svc.getPricingFilePath()))
			require.Equal(t, float64(5), svc.GetModelPricing("fallback-only").InputCostPerToken)
		})
	}
}

func TestPricingFallbackValidationCoversConsumedNumericFields(t *testing.T) {
	entryType := reflect.TypeOf(LiteLLMRawEntry{})
	for i := 0; i < entryType.NumField(); i++ {
		field := entryType.Field(i)
		if field.Type != reflect.TypeOf((*float64)(nil)) && field.Type != reflect.TypeOf((*int)(nil)) {
			continue
		}
		name := field.Tag.Get("json")
		t.Run(name, func(t *testing.T) {
			require.Error(t, validatePricingFallbackEntry(json.RawMessage(`{"`+name+`":-1}`)))
			require.Error(t, validatePricingFallbackEntry(json.RawMessage(`{"`+name+`":"free"}`)))
			require.NoError(t, validatePricingFallbackEntry(json.RawMessage(`{"`+name+`":0}`)))
			require.NoError(t, validatePricingFallbackEntry(json.RawMessage(`{"`+name+`":null}`)))
		})
	}
}

func TestPricingFallbackValidationAcceptsBundledCatalog(t *testing.T) {
	svc := pricingSyncFixture(t, nil)
	svc.cfg.Pricing.FallbackFile = "../../resources/model-pricing/model_prices_and_context_window.json"
	require.NoError(t, svc.validateCustomPricingFiles(), "nested price metadata must remain compatible")
}

func TestPricingInvalidStartupFallbackKeepsLastGoodFile(t *testing.T) {
	svc := pricingSyncFixture(t, nil)
	fallback := filepath.Join(svc.cfg.Pricing.DataDir, "fallback.json")
	svc.cfg.Pricing.FallbackFile = fallback
	lastGood := []byte(`{"test-model":{"input_cost_per_token":2}}`)
	require.NoError(t, os.WriteFile(svc.getPricingFilePath(), lastGood, 0600))
	require.NoError(t, os.WriteFile(fallback, []byte(`{"test-model":{"input_cost_per_token":-1}}`), 0600))
	require.Error(t, svc.useFallbackPricing())
	got, err := os.ReadFile(svc.getPricingFilePath())
	require.NoError(t, err)
	require.Equal(t, lastGood, got, "invalid fallback must not replace the last valid disk catalog")
}
