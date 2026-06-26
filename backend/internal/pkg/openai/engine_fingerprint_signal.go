package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

type EngineFingerprintSignal struct {
	Type     string   `json:"type"`
	Match    []string `json:"match"`
	Required bool     `json:"required"`
}

const (
	FingerprintSignalHeaderExact  = "header_exact"
	FingerprintSignalHeaderPrefix = "header_prefix"
	FingerprintSignalBodyPath     = "body_path"
)

var DefaultEngineFingerprintSignals = []EngineFingerprintSignal{
	{Type: FingerprintSignalHeaderPrefix, Match: []string{"x-codex-"}, Required: true},
	{Type: FingerprintSignalHeaderExact, Match: []string{"session-id", "session_id"}, Required: false},
	{Type: FingerprintSignalHeaderExact, Match: []string{"thread-id", "thread_id"}, Required: false},
	{Type: FingerprintSignalBodyPath, Match: []string{"client_metadata.x-codex-window-id", "client_metadata.x-codex-installation-id"}, Required: false},
}

func EvaluateEngineFingerprint(h http.Header, body []byte, signals []EngineFingerprintSignal) bool {
	for _, signal := range signals {
		if !signal.Required {
			continue
		}
		if !engineSignalMatches(h, body, signal) {
			return false
		}
	}
	return true
}

func engineSignalMatches(h http.Header, body []byte, signal EngineFingerprintSignal) bool {
	switch signal.Type {
	case FingerprintSignalHeaderExact:
		for _, name := range signal.Match {
			if n := strings.TrimSpace(name); n != "" && h != nil && strings.TrimSpace(h.Get(n)) != "" {
				return true
			}
		}
	case FingerprintSignalHeaderPrefix:
		if h == nil {
			return false
		}
		for key := range h {
			lowerKey := strings.ToLower(key)
			for _, prefix := range signal.Match {
				if p := strings.ToLower(strings.TrimSpace(prefix)); p != "" && strings.HasPrefix(lowerKey, p) {
					return true
				}
			}
		}
	case FingerprintSignalBodyPath:
		if len(body) == 0 {
			return false
		}
		for _, path := range signal.Match {
			if p := strings.TrimSpace(path); p != "" && gjson.GetBytes(body, p).Exists() {
				return true
			}
		}
	}
	return false
}

func ParseEngineFingerprintSignals(raw string) ([]EngineFingerprintSignal, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, true
	}
	var signals []EngineFingerprintSignal
	if json.Unmarshal([]byte(raw), &signals) != nil {
		return nil, false
	}
	return signals, true
}

func ValidateEngineFingerprintSignalsJSON(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var signals []EngineFingerprintSignal
	if err := json.Unmarshal([]byte(trimmed), &signals); err != nil {
		return fmt.Errorf("must be empty or a valid JSON array of {type, match[], required}")
	}
	for i, signal := range signals {
		switch signal.Type {
		case FingerprintSignalHeaderExact, FingerprintSignalHeaderPrefix, FingerprintSignalBodyPath:
		default:
			return fmt.Errorf("entry %d: type must be one of header_exact/header_prefix/body_path", i)
		}
		hasMatch := false
		for _, m := range signal.Match {
			if strings.TrimSpace(m) != "" {
				hasMatch = true
				break
			}
		}
		if !hasMatch {
			return fmt.Errorf("entry %d: match must contain at least one non-empty value", i)
		}
	}
	return nil
}

func DefaultEngineFingerprintSignalsJSON() string {
	b, _ := json.Marshal(DefaultEngineFingerprintSignals)
	return string(b)
}
