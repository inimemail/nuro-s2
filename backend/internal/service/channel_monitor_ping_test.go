package service

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPingEndpointOriginForMonitorUsesOneSampleByDefault(t *testing.T) {
	calls := 0
	latency := pingEndpointOriginForMonitorWithSampler(context.Background(), "https://example.com", nil, func(context.Context, string) *int {
		calls++
		value := 17
		return &value
	})

	require.NotNil(t, latency)
	require.Equal(t, 17, *latency)
	require.Equal(t, 1, calls)
}

func TestPingEndpointOriginForMonitorUsesMedianForDedicatedProfile(t *testing.T) {
	values := []int{48, 17, 31}
	var mu sync.Mutex
	calls := 0
	sample := func(context.Context, string) *int {
		mu.Lock()
		defer mu.Unlock()
		value := values[calls]
		calls++
		return &value
	}
	opts := &CheckOptions{ExtraHeaders: map[string]string{
		strings.ToLower(OpenAIHealthProbeHeader): strings.ToUpper(OpenAIHealthProbeProfileResponsesV1),
	}}
	opts = dedicatedHealthProbeCheckOptions(opts.ExtraHeaders)

	latency := pingEndpointOriginForMonitorWithSampler(context.Background(), "https://example.com", opts, sample)

	require.NotNil(t, latency)
	require.Equal(t, 31, *latency)
	require.Equal(t, 3, calls)
}

func TestPingEndpointOriginForMonitorIgnoresFailedDedicatedSamples(t *testing.T) {
	values := []*int{intPointer(40), nil, intPointer(18)}
	var mu sync.Mutex
	calls := 0
	sample := func(context.Context, string) *int {
		mu.Lock()
		defer mu.Unlock()
		value := values[calls]
		calls++
		return value
	}
	opts := dedicatedHealthProbeCheckOptions(map[string]string{
		OpenAIHealthProbeHeader: OpenAIHealthProbeProfileResponsesV1,
	})

	latency := pingEndpointOriginForMonitorWithSampler(context.Background(), "https://example.com", opts, sample)

	require.NotNil(t, latency)
	require.Equal(t, 29, *latency)
	require.Equal(t, 3, calls)
}

func TestPingEndpointOriginForMonitorReturnsNilWhenAllDedicatedSamplesFail(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	opts := dedicatedHealthProbeCheckOptions(map[string]string{
		OpenAIHealthProbeHeader: OpenAIHealthProbeProfileResponsesV1,
	})
	latency := pingEndpointOriginForMonitorWithSampler(context.Background(), "https://example.com", opts, func(context.Context, string) *int {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil
	})

	require.Nil(t, latency)
	require.Equal(t, 3, calls)
}

func TestPingEndpointOriginForMonitorHeaderAloneKeepsDefaultSampling(t *testing.T) {
	calls := 0
	opts := &CheckOptions{ExtraHeaders: map[string]string{
		OpenAIHealthProbeHeader: OpenAIHealthProbeProfileResponsesV1,
	}}
	latency := pingEndpointOriginForMonitorWithSampler(context.Background(), "https://example.com", opts, func(context.Context, string) *int {
		calls++
		return intPointer(23)
	})

	require.NotNil(t, latency)
	require.Equal(t, 23, *latency)
	require.Equal(t, 1, calls)
}

func dedicatedHealthProbeCheckOptions(headers map[string]string) *CheckOptions {
	return &CheckOptions{
		APIMode:          MonitorAPIModeResponses,
		ExtraHeaders:     headers,
		BodyOverrideMode: MonitorBodyOverrideModeReplace,
		BodyOverride: map[string]any{
			"model":             "gpt-5.5",
			"instructions":      openAIHealthProbeInstructions,
			"input":             openAIHealthProbeInput,
			"reasoning":         map[string]any{"effort": "none"},
			"max_output_tokens": openAIHealthProbeMaxOutputTokens,
			"stream":            false,
			"store":             false,
		},
	}
}

func intPointer(value int) *int {
	return &value
}
