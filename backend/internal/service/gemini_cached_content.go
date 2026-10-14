package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	ErrGeminiCacheUnavailable    = errors.New("Gemini cache service is not ready")
	ErrGeminiCacheNotFound       = errors.New("cached content not found")
	ErrGeminiCacheConflict       = errors.New("cache operation conflict")
	ErrGeminiCacheQuota          = errors.New("insufficient cache operation quota")
	ErrGeminiCacheLease          = errors.New("cache operation lease lost")
	ErrGeminiCacheRejected       = errors.New("cache operation was released without execution")
	ErrGeminiCachePermission     = errors.New("cached content permission is unavailable")
	ErrGeminiCacheInvalidRequest = errors.New("invalid cache management request")
	cachePublicIDPattern         = regexp.MustCompile(`^nuro_[a-f0-9]{32}$`)
)

type GeminiCacheOwner struct{ UserID, APIKeyID, GroupID int64 }

type GeminiCachePricingContext struct {
	Version                    int       `json:"version"`
	PricingAt                  time.Time `json:"pricing_at"`
	Source                     string    `json:"source"`
	ChannelID                  *int64    `json:"channel_id,omitempty"`
	ProviderProject            string    `json:"provider_project,omitempty"`
	ProviderLocation           string    `json:"provider_location,omitempty"`
	InputPerToken              string    `json:"input_per_token"`
	StoragePerTokenHour        string    `json:"storage_per_token_hour"`
	UserMultiplier             string    `json:"user_multiplier"`
	AccountMultiplier          string    `json:"account_multiplier"`
	AccountInputPerToken       string    `json:"account_input_per_token,omitempty"`
	AccountStoragePerTokenHour string    `json:"account_storage_per_token_hour,omitempty"`
	AccountSource              string    `json:"account_source,omitempty"`
}

type GeminiCacheQuote struct {
	Pricing       GeminiCachePricingContext `json:"pricing"`
	Tokens        int64                     `json:"tokens"`
	TokenSource   string                    `json:"token_source"`
	InputCost     string                    `json:"input_cost"`
	StorageCost   string                    `json:"storage_cost"`
	Amount        string                    `json:"amount"`
	AccountAmount string                    `json:"account_amount"`
	PaidFrom      time.Time                 `json:"paid_from"`
	PaidUntil     time.Time                 `json:"paid_until"`
}

type GeminiCachedContent struct {
	ID             string                    `json:"-"`
	Owner          GeminiCacheOwner          `json:"-"`
	AccountID      int64                     `json:"-"`
	RequestedModel string                    `json:"-"`
	UpstreamModel  string                    `json:"-"`
	BillingModel   string                    `json:"-"`
	UpstreamName   string                    `json:"-"`
	DisplayName    string                    `json:"displayName,omitempty"`
	State          string                    `json:"state"`
	TokenCount     int64                     `json:"-"`
	ExpiresAt      time.Time                 `json:"expireTime"`
	PaidUntil      time.Time                 `json:"-"`
	Pricing        GeminiCachePricingContext `json:"-"`
	Revision       int64                     `json:"-"`
	CreatedAt      time.Time                 `json:"createTime"`
	UpdatedAt      time.Time                 `json:"updateTime"`
	Billing        GeminiCacheBillingSummary `json:"billing"`
}

type GeminiCacheBillingSummary struct {
	ReservedAmount string `json:"reservedAmount"`
	SettledAmount  string `json:"settledAmount"`
	InputCost      string `json:"inputCost"`
	StorageCost    string `json:"storageCost"`
	OperationState string `json:"operationState"`
}

func (r GeminiCachedContent) PublicResponse() map[string]any {
	return map[string]any{"name": "cachedContents/" + r.ID, "displayName": r.DisplayName,
		"model": "models/" + r.RequestedModel, "expireTime": r.ExpiresAt.UTC(), "createTime": r.CreatedAt.UTC(),
		"updateTime": r.UpdatedAt.UTC(), "usageMetadata": map[string]any{"totalTokenCount": r.TokenCount}, "state": r.State, "billing": r.Billing}
}

type GeminiCacheOperation struct {
	ID, ResourceID, Kind, IdempotencyKeyHash, Fingerprint, State string
	Owner                                                        GeminiCacheOwner
	AccountID                                                    int64
	SubscriptionID                                               *int64
	BaselineExpiry                                               *time.Time
	TargetExpiry                                                 time.Time
	Quote                                                        GeminiCacheQuote
	UpstreamName                                                 string
	ConfirmedTokens                                              *int64
	ConfirmedExpiry                                              *time.Time
	SendStartedAt                                                *time.Time
	Generation                                                   int64
	LeaseOwner                                                   string
	LeaseUntil                                                   *time.Time
	Attempts                                                     int
	LastErrorCode                                                string
}

type GeminiCacheBinding struct {
	ID                                                 string
	Owner                                              GeminiCacheOwner
	AccountID                                          int64
	RequestedModel, UpstreamModel, UpstreamName, State string
	ExpiresAt                                          time.Time
	Revision                                           int64
}

type GeminiCacheViewBatch struct {
	Revision                int64
	Bindings                []GeminiCacheBinding
	Complete                bool
	Reset                   bool
	NextAuthorizationChange *time.Time
}

type GeminiCachedContentRepository interface {
	Prepare(context.Context, *GeminiCachedContent, *GeminiCacheOperation, int) (*GeminiCacheOperation, error)
	GetForOwner(context.Context, GeminiCacheOwner, string) (*GeminiCachedContent, error)
	ListForOwner(context.Context, GeminiCacheOwner, string, int) ([]GeminiCachedContent, string, error)
	GetOperation(context.Context, string) (*GeminiCacheOperation, error)
	FindOperation(context.Context, GeminiCacheOwner, string, string) (*GeminiCacheOperation, error)
	MarkSendStarted(context.Context, *GeminiCacheOperation) error
	Reject(context.Context, *GeminiCacheOperation) error
	Confirm(context.Context, *GeminiCacheOperation, string, int64, time.Time) error
	Finish(context.Context, *GeminiCacheOperation, string) error
	Retry(context.Context, *GeminiCacheOperation, string, time.Time, bool) error
	ClaimRecovery(context.Context, string, int, time.Duration) ([]GeminiCacheOperation, error)
	ReadView(context.Context, int64, int, bool) (*GeminiCacheViewBatch, error)
	LeaseView(context.Context, string, int64, time.Duration) error
	Barrier(context.Context, int64) (bool, error)
}

type GeminiCacheChargeRepository interface {
	ReserveCacheCharge(context.Context, *GeminiCacheOperation) error
	CaptureCacheCharge(context.Context, *GeminiCacheOperation, GeminiCacheQuote) error
	ReleaseCacheCharge(context.Context, *GeminiCacheOperation) error
}

type GeminiCacheCreateRequest struct {
	Model       string
	DisplayName string
	ExpiresAt   time.Time
	Body        map[string]json.RawMessage
}

func ValidGeminiCachePublicID(id string) bool { return cachePublicIDPattern.MatchString(id) }

func newGeminiCacheID() string { return "nuro_" + strings.ReplaceAll(uuid.NewString(), "-", "") }

func parseGeminiCacheJSON(body []byte) (map[string]json.RawMessage, error) {
	if len(body) == 0 || len(body) > 8<<20 || !utf8.Valid(body) || !json.Valid(body) || !uniqueJSONValue(json.NewDecoder(bytes.NewReader(body)), 0) {
		return nil, errors.New("invalid cached content JSON")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, errors.New("cached content must be an object")
	}
	return fields, nil
}

func parseGeminiCacheExpiry(fields map[string]json.RawMessage, now time.Time, maxTTL time.Duration) (time.Time, error) {
	ttl, hasTTL := fields["ttl"]
	expiry, hasExpiry := fields["expireTime"]
	if hasTTL && hasExpiry {
		return time.Time{}, errors.New("use ttl or expireTime, not both")
	}
	if maxTTL <= 0 || maxTTL > 24*time.Hour {
		return time.Time{}, errors.New("invalid cache TTL limit")
	}
	target := now.Add(min(time.Hour, maxTTL))
	if hasTTL {
		var value string
		if json.Unmarshal(ttl, &value) != nil || !strings.HasSuffix(value, "s") {
			return time.Time{}, errors.New("invalid cache ttl")
		}
		seconds, err := decimal.NewFromString(strings.TrimSuffix(value, "s"))
		if err != nil || !seconds.IsPositive() || seconds.GreaterThan(decimal.NewFromInt(int64(maxTTL/time.Second))) {
			return time.Time{}, errors.New("cache ttl is outside the allowed range")
		}
		nanos := seconds.Mul(decimal.NewFromInt(int64(time.Second))).Ceil()
		target = now.Add(time.Duration(nanos.IntPart()))
	}
	if hasExpiry {
		var value string
		if json.Unmarshal(expiry, &value) != nil {
			return time.Time{}, errors.New("invalid expireTime")
		}
		var err error
		target, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, errors.New("invalid expireTime")
		}
	}
	if !target.After(now) || target.Sub(now) > maxTTL {
		return time.Time{}, errors.New("cache expiry is outside the allowed range")
	}
	return target.UTC(), nil
}

func ParseGeminiCacheCreate(body []byte, now time.Time, maxTTL time.Duration) (*GeminiCacheCreateRequest, error) {
	fields, err := parseGeminiCacheJSON(body)
	if err != nil {
		return nil, err
	}
	for field := range fields {
		switch field {
		case "model", "displayName", "contents", "systemInstruction", "tools", "toolConfig", "ttl", "expireTime":
		default:
			return nil, fmt.Errorf("unsupported cache field: %s", field)
		}
	}
	var model, name string
	if json.Unmarshal(fields["model"], &model) != nil {
		return nil, errors.New("cache model is required")
	}
	model = strings.TrimPrefix(model, "models/")
	if !IsSafeGeminiModelPathSegment(model) || !strings.HasPrefix(model, "gemini-") {
		return nil, errors.New("invalid cache model")
	}
	if raw, exists := fields["displayName"]; exists && json.Unmarshal(raw, &name) != nil {
		return nil, errors.New("invalid displayName")
	}
	if len(name) > 128 {
		return nil, errors.New("displayName is too long")
	}
	if _, contents := fields["contents"]; !contents {
		if _, system := fields["systemInstruction"]; !system {
			return nil, errors.New("cached content is empty")
		}
	}
	if err := validateGeminiCachePayload(fields); err != nil {
		return nil, err
	}
	target, err := parseGeminiCacheExpiry(fields, now, maxTTL)
	if err != nil {
		return nil, err
	}
	return &GeminiCacheCreateRequest{Model: model, DisplayName: name, ExpiresAt: target, Body: fields}, nil
}

func ParseGeminiCachePatch(body []byte, mask string, now time.Time, maxTTL time.Duration) (time.Time, error) {
	fields, err := parseGeminiCacheJSON(body)
	if err != nil {
		return time.Time{}, err
	}
	if len(fields) != 1 {
		return time.Time{}, errors.New("only expiry can be updated")
	}
	for key := range fields {
		if key != "ttl" && key != "expireTime" {
			return time.Time{}, errors.New("only expiry can be updated")
		}
		if mask != "" && mask != key && !(key == "expireTime" && mask == "expire_time") {
			return time.Time{}, errors.New("invalid cache update mask")
		}
	}
	return parseGeminiCacheExpiry(fields, now, maxTTL)
}

func GeminiCacheFingerprint(owner GeminiCacheOwner, kind, resource string, body []byte) string {
	sum := sha256.Sum256(append([]byte(fmt.Sprintf("%d|%d|%d|%s|%s|", owner.UserID, owner.APIKeyID, owner.GroupID, kind, resource)), body...))
	return hex.EncodeToString(sum[:])
}

func GeminiCacheIdempotencyHash(key string) (string, error) {
	if key == "" {
		return "", nil
	}
	if len(key) > 128 || !utf8.ValidString(key) || strings.ContainsAny(key, "\r\n\x00") {
		return "", errors.New("invalid idempotency key")
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), nil
}

func cacheMoney(value string, scale int32) (decimal.Decimal, error) {
	if len(value) == 0 || len(value) > 64 {
		return decimal.Zero, errors.New("invalid cache money")
	}
	amount, err := decimal.NewFromString(value)
	if err != nil || amount.IsNegative() || amount.GreaterThanOrEqual(decimal.NewFromInt(10000000000)) {
		return decimal.Zero, errors.New("cache money is outside numeric bounds")
	}
	return amount, nil
}

func QuoteGeminiCache(pricing GeminiCachePricingContext, tokens int64, from, until time.Time, includeInput bool) (GeminiCacheQuote, error) {
	if tokens <= 0 || tokens > 1_000_000_000 || until.Before(from) || until.Sub(from) > 24*time.Hour {
		return GeminiCacheQuote{}, errors.New("invalid cache quote basis")
	}
	input, err := cacheMoney(pricing.InputPerToken, 10)
	if err != nil {
		return GeminiCacheQuote{}, err
	}
	storage, err := cacheMoney(pricing.StoragePerTokenHour, 10)
	if err != nil {
		return GeminiCacheQuote{}, errors.New("cache storage price is unavailable")
	}
	multiplier, err := cacheMoney(pricing.UserMultiplier, 10)
	if err != nil || multiplier.IsNegative() {
		return GeminiCacheQuote{}, errors.New("invalid cache multiplier")
	}
	count := decimal.NewFromInt(tokens)
	inputCost := decimal.Zero
	if includeInput {
		inputCost = count.Mul(input).Mul(multiplier)
	}
	storageCost := count.Mul(storage).Mul(multiplier).Mul(decimal.NewFromInt(until.Sub(from).Nanoseconds())).Div(decimal.NewFromInt(int64(time.Hour)))
	accountMultiplier, err := cacheMoney(pricing.AccountMultiplier, 10)
	if err != nil || accountMultiplier.IsNegative() {
		return GeminiCacheQuote{}, errors.New("invalid cache account multiplier")
	}
	accountInput, accountStorage := input, storage
	if pricing.Version >= 2 {
		accountInput, err = cacheMoney(pricing.AccountInputPerToken, 10)
		if err != nil {
			return GeminiCacheQuote{}, err
		}
		accountStorage, err = cacheMoney(pricing.AccountStoragePerTokenHour, 10)
		if err != nil {
			return GeminiCacheQuote{}, err
		}
	}
	accountCost := count.Mul(accountStorage).Mul(decimal.NewFromInt(until.Sub(from).Nanoseconds())).Div(decimal.NewFromInt(int64(time.Hour)))
	if includeInput {
		accountCost = accountCost.Add(count.Mul(accountInput))
	}
	accountCost = accountCost.Mul(accountMultiplier).RoundCeil(8)
	if _, err := cacheMoney(accountCost.String(), 8); err != nil {
		return GeminiCacheQuote{}, err
	}
	amount := inputCost.Add(storageCost).RoundCeil(8)
	if _, err := cacheMoney(amount.String(), 8); err != nil {
		return GeminiCacheQuote{}, err
	}
	return GeminiCacheQuote{Pricing: pricing, Tokens: tokens, TokenSource: "provider_count_tokens", InputCost: inputCost.RoundCeil(10).StringFixed(10), StorageCost: storageCost.RoundCeil(10).StringFixed(10), Amount: amount.StringFixed(8), AccountAmount: accountCost.StringFixed(8), PaidFrom: from.UTC(), PaidUntil: until.UTC()}, nil
}

func SameGeminiCachePricing(a, b GeminiCachePricingContext) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
