package service

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

var channelTimePricingLocations sync.Map

type parsedChannelTimePeriod struct {
	start, end int
	multiplier float64
}

func validateChannelTimePricing(config *ChannelTimePricing) error {
	if config == nil || len(config.Periods) == 0 {
		return nil
	}
	if _, err := loadChannelTimePricingLocation(config.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	_, err := parseChannelTimePeriods(config.Periods)
	return err
}

func (config *ChannelTimePricing) MultiplierAt(at time.Time) float64 {
	if config == nil || len(config.Periods) == 0 || at.IsZero() || validateChannelTimePricing(config) != nil {
		return 1
	}
	location, err := loadChannelTimePricingLocation(config.Timezone)
	if err != nil {
		return 1
	}
	periods, err := parseChannelTimePeriods(config.Periods)
	if err != nil {
		return 1
	}
	local := at.In(location)
	second := local.Hour()*3600 + local.Minute()*60 + local.Second()
	for _, period := range periods {
		if second >= period.start && second < period.end {
			return period.multiplier
		}
	}
	return 1
}

func loadChannelTimePricingLocation(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("timezone must be an IANA location")
	}
	if cached, ok := channelTimePricingLocations.Load(name); ok {
		if location, ok := cached.(*time.Location); ok {
			return location, nil
		}
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	actual, _ := channelTimePricingLocations.LoadOrStore(name, location)
	result, ok := actual.(*time.Location)
	if !ok {
		return nil, fmt.Errorf("invalid cached timezone %q", name)
	}
	return result, nil
}

func parseChannelTime(value string, end bool) (int, error) {
	if end && (value == "00:00" || value == "00:00:00") {
		return 24 * 3600, nil
	}
	layout := "15:04:05"
	if len(value) == 5 {
		layout = "15:04"
	}
	parsed, err := time.Parse(layout, value)
	if err != nil || parsed.Format(layout) != value {
		return 0, fmt.Errorf("time %q must use HH:mm or HH:mm:ss format", value)
	}
	return parsed.Hour()*3600 + parsed.Minute()*60 + parsed.Second(), nil
}

func parseChannelTimePeriods(periods []ChannelTimePricingPeriod) ([]parsedChannelTimePeriod, error) {
	parsed := make([]parsedChannelTimePeriod, 0, len(periods))
	for _, period := range periods {
		if math.IsNaN(period.Multiplier) || math.IsInf(period.Multiplier, 0) || period.Multiplier < 0.01 {
			return nil, fmt.Errorf("multiplier must be finite and at least 0.01")
		}
		scaled := period.Multiplier * 100
		if math.IsInf(scaled, 0) || math.Abs(scaled-math.Round(scaled)) > 1e-9 {
			return nil, fmt.Errorf("multiplier must have at most two decimal places")
		}
		start, err := parseChannelTime(period.StartTime, false)
		if err != nil {
			return nil, err
		}
		end, err := parseChannelTime(period.EndTime, true)
		if err != nil {
			return nil, err
		}
		if start >= end {
			return nil, fmt.Errorf("start time must be before end time")
		}
		parsed = append(parsed, parsedChannelTimePeriod{start: start, end: end, multiplier: period.Multiplier})
	}
	sort.Slice(parsed, func(i, j int) bool { return parsed[i].start < parsed[j].start })
	for i := 1; i < len(parsed); i++ {
		if parsed[i].start < parsed[i-1].end {
			return nil, fmt.Errorf("time pricing periods overlap")
		}
	}
	return parsed, nil
}
