package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

var errAnthropicCompatDrainTimeout = errors.New("client disconnect drain deadline exceeded")

func (s *GatewayService) compatStreamInterval() time.Duration {
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		return time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	return 0
}

// nextAnthropicCompatFrame accepts SSE comments and multiline data without
// allowing the total frame size to exceed the configured line-size budget.
func nextAnthropicCompatFrame(pump *anthropicNativeLinePump, maxBytes int) (string, string, error) {
	var name string
	var data strings.Builder
	for {
		line, err := pump.next()
		if err != nil {
			if errors.Is(err, io.EOF) && data.Len() > 0 {
				return name, data.String(), nil
			}
			return "", "", err
		}
		if line == "" {
			if data.Len() > 0 {
				return name, data.String(), nil
			}
			name = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if field, ok := parseAnthropicSSEField(line, "event"); ok {
			name = strings.TrimSpace(field)
		}
		if field, ok := parseAnthropicSSEField(line, "data"); ok {
			if data.Len()+len(field)+1 > maxBytes {
				return "", "", errors.New("upstream SSE frame exceeds limit")
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(field)
		}
	}
}

func gatewayCompatClientContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

// anthropicCompatStartsRealOutput deliberately excludes pings, empty
// keepalives and signature-only deltas from TTFT. They keep a stream alive but
// are not output a downstream client can consume as content or tool arguments.
func anthropicCompatStartsRealOutput(event *apicompat.AnthropicStreamEvent) bool {
	if event == nil {
		return false
	}
	if event.Type == "content_block_start" && event.ContentBlock != nil {
		return event.ContentBlock.Text != "" || event.ContentBlock.Thinking != "" || event.ContentBlock.Type == "tool_use"
	}
	if event.Type != "content_block_delta" || event.Delta == nil {
		return false
	}
	switch event.Delta.Type {
	case "text_delta":
		return event.Delta.Text != ""
	case "thinking_delta":
		return event.Delta.Thinking != ""
	case "input_json_delta":
		return event.Delta.PartialJSON != ""
	default:
		return false
	}
}
