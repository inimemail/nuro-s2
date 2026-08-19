package service

import (
	"testing"
	"time"
)

func TestChannelTimePricingMultiplierAt(t *testing.T) {
	config := &ChannelTimePricing{Timezone: "UTC", Periods: []ChannelTimePricingPeriod{{StartTime: "09:00", EndTime: "17:00", Multiplier: 1.5}}}
	inside := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	outside := time.Date(2026, 8, 18, 18, 0, 0, 0, time.UTC)
	if got := config.MultiplierAt(inside); got != 1.5 {
		t.Fatalf("inside multiplier = %v", got)
	}
	if got := config.MultiplierAt(outside); got != 1 {
		t.Fatalf("outside multiplier = %v", got)
	}
}

func TestChannelTimePricingRejectsOverlapAndInvalidTimezone(t *testing.T) {
	if err := validateChannelTimePricing(&ChannelTimePricing{Timezone: "UTC", Periods: []ChannelTimePricingPeriod{{StartTime: "09:00", EndTime: "12:00", Multiplier: 1}, {StartTime: "11:00", EndTime: "13:00", Multiplier: 1}}}); err == nil {
		t.Fatal("expected overlap error")
	}
	if err := validateChannelTimePricing(&ChannelTimePricing{Timezone: "not/a-zone", Periods: []ChannelTimePricingPeriod{{StartTime: "09:00", EndTime: "10:00", Multiplier: 1}}}); err == nil {
		t.Fatal("expected timezone error")
	}
}
