package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIProxyStreamCircuitTripsAndExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	circuit := newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
		enabled:          true,
		failureThreshold: 2,
		failureWindow:    time.Minute,
		quarantineTTL:    10 * time.Minute,
		maxEntries:       2,
	})

	tripped, _ := circuit.recordFailure(7, now)
	require.False(t, tripped)
	tripped, until := circuit.recordFailure(7, now.Add(time.Second))
	require.True(t, tripped)
	require.True(t, circuit.isBlocked(7, now.Add(2*time.Second)))
	require.False(t, circuit.isBlocked(7, until))
}

func TestOpenAIProxyStreamCircuitCollapsesBurstFailures(t *testing.T) {
	now := time.Unix(1000, 0)
	circuit := newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
		enabled:          true,
		failureThreshold: 2,
		failureWindow:    time.Minute,
		quarantineTTL:    10 * time.Minute,
		collapseInterval: 3 * time.Second,
	})

	tripped, _ := circuit.recordFailure(8, now)
	require.False(t, tripped)
	tripped, _ = circuit.recordFailure(8, now.Add(time.Second))
	require.False(t, tripped, "failures in one burst must count once")
	tripped, _ = circuit.recordFailure(8, now.Add(4*time.Second))
	require.True(t, tripped)
}

func TestOpenAIProxyStreamCircuitDoesNotCollapseAcrossFailureWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	circuit := newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
		enabled:          true,
		failureThreshold: 2,
		failureWindow:    time.Second,
		quarantineTTL:    time.Minute,
		collapseInterval: 3 * time.Second,
	})

	tripped, _ := circuit.recordFailure(9, now)
	require.False(t, tripped)
	tripped, _ = circuit.recordFailure(9, now.Add(1500*time.Millisecond))
	require.False(t, tripped, "the first failure in a new window must not be collapsed")
	circuit.mu.Lock()
	require.Equal(t, 1, circuit.entries[9].failureCount)
	circuit.mu.Unlock()
}

func TestOpenAIProxyStreamCircuitIgnoresNonUpstreamFailures(t *testing.T) {
	circuit := newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{enabled: true})
	service := &OpenAIGatewayService{}
	account := &Account{Platform: PlatformOpenAI, ProxyID: ptrInt64ForCircuit(9)}
	service.openaiProxyStreamCircuit = circuit
	service.openaiProxyStreamCircuitOnce.Do(func() {})

	service.recordOpenAIProxyStreamOutcome(account, OpenAIStreamFailureClientCancelled, true, false, errCircuitTest)
	service.recordOpenAIProxyStreamOutcome(account, OpenAIStreamFailureLocalCapacityRejected, true, false, errCircuitTest)
	require.False(t, circuit.isBlocked(9, time.Now()))
}

func TestOpenAIProxyStreamCircuitCountsDisconnectBeforeClientOutput(t *testing.T) {
	circuit := newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
		enabled:          true,
		failureThreshold: 2,
		failureWindow:    time.Minute,
		quarantineTTL:    10 * time.Minute,
	})
	service := &OpenAIGatewayService{}
	service.openaiProxyStreamCircuit = circuit
	service.openaiProxyStreamCircuitOnce.Do(func() {})
	account := &Account{Platform: PlatformOpenAI, ProxyID: ptrInt64ForCircuit(11)}
	service.recordOpenAIProxyStreamOutcome(account, OpenAIStreamFailureUpstreamDisconnect, false, false, errCircuitTest)
	service.recordOpenAIProxyStreamOutcome(account, OpenAIStreamFailureUpstreamDisconnect, false, false, errCircuitTest)
	require.True(t, circuit.isBlocked(11, time.Now()))
}

func TestOpenAIProxyStreamCircuitCountsEdgeDisconnectWithoutErrorText(t *testing.T) {
	circuit := newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
		enabled:          true,
		failureThreshold: 2,
		failureWindow:    time.Minute,
		quarantineTTL:    10 * time.Minute,
	})
	service := &OpenAIGatewayService{}
	service.openaiProxyStreamCircuit = circuit
	service.openaiProxyStreamCircuitOnce.Do(func() {})
	account := &Account{Platform: PlatformOpenAI, ProxyID: ptrInt64ForCircuit(12)}
	service.RecordOpenAIEdgeStreamOutcome(account, "upstream_disconnect", true, false, "")
	service.RecordOpenAIEdgeStreamOutcome(account, "upstream_disconnect", true, false, "")
	require.True(t, circuit.isBlocked(12, time.Now()))
}

func TestOpenAIProxyStreamCircuitTerminalUpstreamErrorBreaksConsecutiveDisconnects(t *testing.T) {
	circuit := newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
		enabled:          true,
		failureThreshold: 2,
		failureWindow:    time.Minute,
		quarantineTTL:    10 * time.Minute,
	})
	service := &OpenAIGatewayService{}
	service.openaiProxyStreamCircuit = circuit
	service.openaiProxyStreamCircuitOnce.Do(func() {})
	account := &Account{Platform: PlatformOpenAI, ProxyID: ptrInt64ForCircuit(13)}
	service.RecordOpenAIEdgeStreamOutcome(account, "upstream_disconnect", true, false, "disconnected")
	service.RecordOpenAIEdgeStreamOutcome(account, "upstream_error", true, false, "Upstream request failed")
	service.RecordOpenAIEdgeStreamOutcome(account, "upstream_disconnect", true, false, "disconnected")
	require.False(t, circuit.isBlocked(13, time.Now()))
}

func TestOpenAIProxyStreamCircuitTerminalDoesNotCancelActiveQuarantine(t *testing.T) {
	now := time.Now()
	circuit := newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
		enabled: true, failureThreshold: 2, failureWindow: time.Minute,
		quarantineTTL: 10 * time.Minute,
	})
	circuit.recordFailure(14, now)
	circuit.recordFailure(14, now.Add(time.Second))
	require.True(t, circuit.isBlocked(14, now.Add(2*time.Second)))

	circuit.recordTerminal(14, now.Add(3*time.Second))
	require.True(t, circuit.isBlocked(14, now.Add(4*time.Second)))
}

func TestOpenAIProxyStreamCircuitDisabledIsNoop(t *testing.T) {
	circuit := newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{enabled: false})
	tripped, _ := circuit.recordFailure(1, time.Now())
	require.False(t, tripped)
	require.False(t, circuit.isBlocked(1, time.Now()))
}

var errCircuitTest = errCircuitTestValue{}

type errCircuitTestValue struct{}

func (errCircuitTestValue) Error() string { return "upstream disconnect" }

func ptrInt64ForCircuit(value int64) *int64 { return &value }
