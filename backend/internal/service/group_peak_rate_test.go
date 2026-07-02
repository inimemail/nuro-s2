package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

func init() {
	_ = timezone.Init("UTC")
}

func newPeakGroup(enabled bool, start, end string, mult float64) *Group {
	return &Group{
		SubscriptionType:   SubscriptionTypeSubscription,
		PeakRateEnabled:    enabled,
		PeakStart:          start,
		PeakEnd:            end,
		PeakRateMultiplier: mult,
	}
}

func peakAt(hour, min int) time.Time {
	return time.Date(2026, 6, 29, hour, min, 0, 0, time.UTC)
}

func TestPeakMultiplierAt_BoundariesAndFallbacks(t *testing.T) {
	cases := []struct {
		name string
		g    *Group
		now  time.Time
		want float64
	}{
		{"nil group", nil, peakAt(15, 0), 1},
		{"disabled", newPeakGroup(false, "14:00", "18:00", 3), peakAt(15, 0), 1},
		{"standard group ignored", &Group{SubscriptionType: SubscriptionTypeStandard, PeakRateEnabled: true, PeakStart: "14:00", PeakEnd: "18:00", PeakRateMultiplier: 3}, peakAt(15, 0), 1},
		{"invalid start", newPeakGroup(true, "99:99", "18:00", 3), peakAt(15, 0), 1},
		{"cross day rejected", newPeakGroup(true, "22:00", "02:00", 3), peakAt(23, 0), 1},
		{"before start", newPeakGroup(true, "14:00", "18:00", 3), peakAt(13, 59), 1},
		{"at start", newPeakGroup(true, "14:00", "18:00", 3), peakAt(14, 0), 3},
		{"inside", newPeakGroup(true, "14:00", "18:00", 3), peakAt(17, 59), 3},
		{"at end", newPeakGroup(true, "14:00", "18:00", 3), peakAt(18, 0), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.g.PeakMultiplierAt(tc.now); got != tc.want {
				t.Fatalf("PeakMultiplierAt() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidatePeakRateConfig(t *testing.T) {
	cases := []struct {
		name    string
		subType string
		enabled bool
		start   string
		end     string
		mult    float64
		wantErr bool
	}{
		{"disabled standard passes", SubscriptionTypeStandard, false, "", "", 0, false},
		{"valid subscription", SubscriptionTypeSubscription, true, "14:00", "18:00", 2.5, false},
		{"zero multiplier allowed", SubscriptionTypeSubscription, true, "14:00", "18:00", 0, false},
		{"standard enabled rejected", SubscriptionTypeStandard, true, "14:00", "18:00", 1, true},
		{"empty start", SubscriptionTypeSubscription, true, "", "18:00", 1, true},
		{"empty end", SubscriptionTypeSubscription, true, "14:00", "", 1, true},
		{"malformed start", SubscriptionTypeSubscription, true, "24:00", "18:00", 1, true},
		{"end before start", SubscriptionTypeSubscription, true, "18:00", "14:00", 1, true},
		{"negative multiplier", SubscriptionTypeSubscription, true, "14:00", "18:00", -0.1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePeakRateConfig(tc.subType, tc.enabled, tc.start, tc.end, tc.mult)
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestPeakMultiplier_GatewayBillingSequence(t *testing.T) {
	const baseMultiplier = 0.8
	approxEq := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	apiKey := &APIKey{Group: newPeakGroup(true, "14:00", "18:00", 3)}

	tokenMultiplier, imageMultiplier := computePeakAwareMultipliers(apiKey, baseMultiplier, peakAt(15, 30))
	if !approxEq(tokenMultiplier, baseMultiplier*3) {
		t.Fatalf("token multiplier = %v, want %v", tokenMultiplier, baseMultiplier*3)
	}
	if !approxEq(imageMultiplier, baseMultiplier) {
		t.Fatalf("image multiplier = %v, want %v", imageMultiplier, baseMultiplier)
	}

	apiKey.Group.ImageRateIndependent = true
	apiKey.Group.ImageRateMultiplier = 0.5
	tokenMultiplier, imageMultiplier = computePeakAwareMultipliers(apiKey, baseMultiplier, peakAt(15, 30))
	if !approxEq(tokenMultiplier, baseMultiplier*3) {
		t.Fatalf("token multiplier with independent image = %v, want %v", tokenMultiplier, baseMultiplier*3)
	}
	if !approxEq(imageMultiplier, 0.5) {
		t.Fatalf("independent image multiplier = %v, want 0.5", imageMultiplier)
	}
}

func TestPeakMultiplier_SnapshotRoundTrip(t *testing.T) {
	apiKey := &APIKey{
		UserID: 1,
		User:   &User{ID: 1, Status: StatusActive, Role: RoleUser},
		Group:  newPeakGroup(true, "14:00", "18:00", 3),
	}
	svc := &APIKeyService{}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	restored := svc.snapshotToAPIKey("key", snapshot)
	if restored == nil || restored.Group == nil {
		t.Fatal("expected restored group")
	}
	if !restored.Group.PeakRateEnabled ||
		restored.Group.PeakStart != "14:00" ||
		restored.Group.PeakEnd != "18:00" ||
		restored.Group.PeakRateMultiplier != 3 {
		t.Fatalf("peak fields lost after snapshot round-trip: %+v", restored.Group)
	}
	if got := restored.Group.PeakMultiplierAt(peakAt(15, 30)); got != 3 {
		t.Fatalf("peak multiplier after round-trip = %v, want 3", got)
	}
}
