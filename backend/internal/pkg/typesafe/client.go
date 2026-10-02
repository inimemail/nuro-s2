package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
)

const (
	DefaultBaseURL = "https://api.typesafe.ai"
	SystemOnePath  = "/v1/systemone"
	JevLatestModel = "jev-latest"
)

const MaxSystemOneResponseBytes = 4 << 20

var ErrSystemOneResponseTooLarge = errors.New("typesafe response exceeds size limit")

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type SystemOneResponse struct {
	Body  []byte
	Model string
	Usage Usage
}

func NewSystemOneRequest(ctx context.Context, baseURL, key string, body []byte) (*http.Request, error) {
	endpoint, err := url.JoinPath(strings.TrimRight(baseURL, "/"), SystemOnePath)
	if err != nil {
		return nil, errors.New("typesafe invalid endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("typesafe invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func DecodeSystemOneResponse(r io.Reader) (*SystemOneResponse, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxSystemOneResponseBytes+1))
	if err != nil {
		return nil, errors.New("typesafe invalid response")
	}
	if len(body) > MaxSystemOneResponseBytes {
		return nil, ErrSystemOneResponseTooLarge
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, errors.New("typesafe invalid response")
	}
	var envelope struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens  json.RawMessage `json:"input_tokens"`
			OutputTokens json.RawMessage `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, errors.New("typesafe invalid response")
	}
	if strings.TrimSpace(envelope.Model) == "" || envelope.Usage == nil {
		return nil, errors.New("typesafe invalid response")
	}
	inputTokens, inputOK := tokenCount(envelope.Usage.InputTokens)
	outputTokens, outputOK := tokenCount(envelope.Usage.OutputTokens)
	if !inputOK || !outputOK {
		return nil, errors.New("typesafe invalid response")
	}
	return &SystemOneResponse{Body: body, Model: envelope.Model, Usage: Usage{
		InputTokens: inputTokens, OutputTokens: outputTokens,
	}}, nil
}

func tokenCount(raw json.RawMessage) (int, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		raw = []byte(strings.TrimSpace(s))
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	v, err := n.Float64()
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	if v != math.Trunc(v) || v > 1<<40 || v > float64(^uint(0)>>1) {
		return 0, false
	}
	return int(v), true
}
