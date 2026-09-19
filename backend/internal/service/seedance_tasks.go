package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const OpenAIEndpointCapabilitySeedance OpenAIEndpointCapability = "seedance"

var (
	ErrSeedanceTaskNotFound = errors.New("Seedance task not found")
	ErrSeedanceCapacity     = errors.New("Seedance asynchronous task capacity reached")
	ErrSeedancePricing      = errors.New("Seedance requires explicit token or per-request pricing")
	ErrSeedanceDuplicate    = errors.New("Seedance operation already exists")
	ErrSeedanceAmbiguousID  = errors.New("Seedance provider task ID is ambiguous; use the local operation ID")
)

// Contains prices and ownership only; never persist upstream/downstream secrets.
type SeedanceSnapshot struct {
	Model                 string
	UpstreamModel         string
	UpstreamBaseURL       string
	CredentialFingerprint string
	BillingModel          string
	BillingMode           BillingMode
	UnitTotal             float64
	UnitActual            float64
	RateMultiplier        float64
	AccountMultiplier     float64
	SubscriptionID        *int64
	KeyQuota              bool
	KeyRateLimit          bool
	AccountQuota          bool
	Platform              string
	InboundEndpoint       string
	PayloadHash           string
	PlatformQuotaFlusher  bool
}

type SeedanceTask struct {
	ID             string
	ProviderID     string
	UserID         int64
	APIKeyID       int64
	AccountID      int64
	GroupID        *int64
	State          string
	Snapshot       SeedanceSnapshot
	Response       json.RawMessage
	CreatedAt      time.Time
	Attempts       int
	Settled        bool
	EffectsPending bool
}

type SeedanceTaskRepository interface {
	Reserve(context.Context, *SeedanceTask, int) error
	Submitted(context.Context, string, string, []byte, string) error
	Owned(context.Context, string, int64, int64, *int64) (*SeedanceTask, error)
	Claim(context.Context) (*SeedanceTask, error)
	Observe(context.Context, string, []byte, string, time.Duration) error
	SaveTerminal(context.Context, *SeedanceTask) error
	Settle(context.Context, *SeedanceTask, *UsageBillingCommand, *UsageLog) (*UsageBillingApplyResult, error)
	CompleteEffects(context.Context, string) error
}
