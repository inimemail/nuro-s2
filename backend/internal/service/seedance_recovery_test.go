package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Return independent copies, as the database does: mutating a claimed task must
// not silently make evidence durable in the test.
type v027SeedanceDurableRepo struct {
	SeedanceTaskRepository
	task                           SeedanceTask
	saveErr                        error
	settleFailures, settles, saves int
}

func (r *v027SeedanceDurableRepo) Claim(context.Context) (*SeedanceTask, error) {
	task := r.task
	task.Response = append([]byte(nil), task.Response...)
	return &task, nil
}
func (r *v027SeedanceDurableRepo) SaveTerminal(_ context.Context, task *SeedanceTask) error {
	r.saves++
	if r.saveErr != nil {
		return r.saveErr
	}
	if _, _, complete := seedanceTaskTerminalUsage(&r.task); !complete {
		r.task.Response = append([]byte(nil), task.Response...)
		r.task.State = task.State
	}
	task.Response = append([]byte(nil), r.task.Response...)
	task.State = r.task.State
	return nil
}
func (r *v027SeedanceDurableRepo) Settle(_ context.Context, task *SeedanceTask, cmd *UsageBillingCommand, usage *UsageLog) (*UsageBillingApplyResult, error) {
	r.settles++
	if r.settleFailures > 0 {
		r.settleFailures--
		return nil, errors.New("temporary billing failure")
	}
	r.task.Settled = true
	return &UsageBillingApplyResult{Applied: true}, nil
}
func (r *v027SeedanceDurableRepo) CompleteEffects(context.Context, string) error { return nil }

func v027SeedanceRecoveryTask() SeedanceTask {
	return SeedanceTask{ID: "local", ProviderID: "task1", CreatedAt: time.Now(), State: "queued", Snapshot: SeedanceSnapshot{UpstreamBaseURL: "https://provider.example", CredentialFingerprint: HashUsageRequestPayload([]byte("sk-upstream")), BillingMode: BillingModeToken, UnitTotal: .1, UnitActual: .2, AccountMultiplier: 1}}
}

func TestV027SeedanceRetriesBillingFromDurableEvidence(t *testing.T) {
	a := newCodexModelsAPIKeyTestAccount("https://provider.example")
	calls := 0
	u := &codexModelsMemoryUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		calls++
		require.Equal(t, http.MethodGet, req.Method)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"task1","status":"succeeded","usage":{"completion_tokens":25}}`))}, nil
	}}
	gateway := newCodexModelsAPIKeyTestService(u)
	gateway.accountRepo = &v027SeedanceOwnerRepo{account: a}
	repo := &v027SeedanceDurableRepo{task: v027SeedanceRecoveryTask(), settleFailures: 1}
	s := NewSeedanceService(repo, gateway, nil)
	s.pollOne(context.Background())
	require.False(t, repo.task.Settled)
	require.Equal(t, 1, repo.settles)
	require.JSONEq(t, `{"id":"task1","status":"succeeded","usage":{"completion_tokens":25}}`, string(repo.task.Response))
	// Even if the original account becomes unusable, known real usage settles.
	a.Credentials["api_key"] = "changed"
	s.pollOne(context.Background())
	require.True(t, repo.task.Settled)
	require.Equal(t, 2, repo.settles)
	require.Equal(t, 1, calls)
}

func TestV027SeedanceDeletePreservesTerminalEvidence(t *testing.T) {
	for _, scenario := range []string{"complete", "save_failed", "missing_usage", "running"} {
		t.Run(scenario, func(t *testing.T) {
			a := newCodexModelsAPIKeyTestAccount("https://provider.example")
			repo := &v027SeedanceDurableRepo{task: v027SeedanceRecoveryTask()}
			if scenario == "save_failed" {
				repo.saveErr = errors.New("database unavailable")
			}
			deletes := 0
			u := &codexModelsMemoryUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				if req.Method == http.MethodDelete {
					deletes++
					if scenario == "complete" {
						require.NotEmpty(t, repo.task.Response)
					}
					return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				body := `{"id":"task1","status":"succeeded","usage":{"completion_tokens":25}}`
				if scenario == "missing_usage" {
					body = `{"id":"task1","status":"succeeded"}`
				}
				if scenario == "running" {
					body = `{"id":"task1","status":"running"}`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			}}
			gateway := newCodexModelsAPIKeyTestService(u)
			gateway.accountRepo = &v027SeedanceOwnerRepo{account: a}
			s := NewSeedanceService(repo, gateway, nil)
			task, err := repo.Claim(context.Background())
			require.NoError(t, err)
			status, _, err := s.Delete(context.Background(), task)
			switch scenario {
			case "complete", "running":
				require.NoError(t, err)
				require.Equal(t, 204, status)
				require.Equal(t, 1, deletes)
			default:
				require.Error(t, err)
				require.Zero(t, deletes)
			}
			if scenario == "complete" {
				s.pollOne(context.Background())
				require.True(t, repo.task.Settled)
			}
			if scenario == "save_failed" {
				s.pollOne(context.Background())
				require.Zero(t, repo.settles)
			}
		})
	}
}

func TestV027SeedanceAbsentDynamicOutputPriceIsNotFree(t *testing.T) {
	pricing := &ResolvedPricing{Mode: BillingModeToken, Source: PricingSourceLiteLLM, BasePricing: &ModelPricing{InputPricePerToken: .1}}
	require.False(t, seedancePricingComplete(&ModelPricingResolver{}, pricing))
}
