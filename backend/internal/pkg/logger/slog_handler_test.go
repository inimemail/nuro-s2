package logger

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type captureState struct {
	writes []capturedWrite
}

type capturedWrite struct {
	fields []zapcore.Field
}

type captureCore struct {
	state      *captureState
	withFields []zapcore.Field
}

func newCaptureCore() *captureCore {
	return &captureCore{state: &captureState{}}
}

func (c *captureCore) Enabled(zapcore.Level) bool {
	return true
}

func (c *captureCore) With(fields []zapcore.Field) zapcore.Core {
	nextFields := make([]zapcore.Field, 0, len(c.withFields)+len(fields))
	nextFields = append(nextFields, c.withFields...)
	nextFields = append(nextFields, fields...)
	return &captureCore{
		state:      c.state,
		withFields: nextFields,
	}
}

func (c *captureCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce.AddCore(entry, c)
}

func (c *captureCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	allFields := make([]zapcore.Field, 0, len(c.withFields)+len(fields))
	allFields = append(allFields, c.withFields...)
	allFields = append(allFields, fields...)
	c.state.writes = append(c.state.writes, capturedWrite{
		fields: allFields,
	})
	return nil
}

func (c *captureCore) Sync() error {
	return nil
}

func TestSlogZapHandler_Handle_DoesNotAppendTimeField(t *testing.T) {
	core := newCaptureCore()
	handler := newSlogZapHandler(zap.New(core))

	record := slog.NewRecord(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), slog.LevelInfo, "hello", 0)
	record.AddAttrs(slog.String("component", "http.access"))

	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("handle slog record: %v", err)
	}
	if len(core.state.writes) != 1 {
		t.Fatalf("write calls = %d, want 1", len(core.state.writes))
	}

	var hasComponent bool
	for _, field := range core.state.writes[0].fields {
		if field.Key == "time" {
			t.Fatalf("unexpected duplicate time field in slog adapter output")
		}
		if field.Key == "component" {
			hasComponent = true
		}
	}
	if !hasComponent {
		t.Fatalf("component field should be preserved")
	}
}

func TestSlogZapHandlerRedactsUpstreamProxyCredentials(t *testing.T) {
	core := newCaptureCore()
	logger := slog.New(newSlogZapHandler(zap.New(core)))
	const proxyURL = "http://example-user:example-password@relay.example:8080"
	logger.Debug("tls_fingerprint_creating_new_client",
		"proxy", proxyURL, "cache_key", "tls:profile:billing_probe:proxy:"+proxyURL, "account_id", 17)
	logger.Debug("tls_fingerprint_request_failed", "error", errors.New("connection to "+proxyURL+" failed"))
	logger.Debug("tls_fingerprint_enabled", "proxy", "relay.example:8080", "target", "api.example")
	logger.Warn("openai_http2_proxy_fallback_activated", "proxy", proxyURL, "fallback_until", "2026-11-25T12:00:00Z")
	logger.Debug("tls_fingerprint_http_proxy_connect_failed_status", "status_code", 407, "status", "407 private proxy diagnostics")
	logger.Debug("tls_fingerprint_request_failed", "error", errors.New("proxy CONNECT failed: 407 private proxy diagnostics"))

	if len(core.state.writes) != 6 {
		t.Fatalf("write calls = %d, want 6", len(core.state.writes))
	}
	for i, expected := range [][]zap.Field{
		{zap.String("proxy", "[REDACTED]"), zap.String("cache_key", "[REDACTED]"), zap.Int64("account_id", 17)},
		{zap.String("error", "[REDACTED]")},
		{zap.String("proxy", "relay.example:8080"), zap.String("target", "api.example")},
		{zap.String("proxy", "[REDACTED]"), zap.String("fallback_until", "2026-11-25T12:00:00Z")},
		{zap.Int64("status_code", 407), zap.String("status", "[REDACTED]")},
		{zap.String("error", "[REDACTED]")},
	} {
		actual := core.state.writes[i].fields
		if len(actual) != len(expected) {
			t.Fatalf("write %d contains %d fields, want %d", i, len(actual), len(expected))
		}
		for fieldIndex, field := range expected {
			if !actual[fieldIndex].Equals(field) {
				t.Errorf("write %d field %q did not match the expected sanitized field", i, field.Key)
			}
		}
	}
}
