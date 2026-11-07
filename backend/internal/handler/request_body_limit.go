package handler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/config"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
)

// A socket read deadline may also cancel the H1 request context. Classify
// the actual read failure first so a stalled upload remains a 408.
func requestBodyReadFailure(req *http.Request, err error) (int, string) {
	if maxErr, ok := extractMaxBytesError(err); ok {
		return http.StatusRequestEntityTooLarge, buildBodyTooLargeMessage(maxErr.Limit)
	}
	if errors.Is(err, pkghttputil.ErrBodyBudgetExceeded) {
		return http.StatusServiceUnavailable, "Request body capacity is temporarily exhausted"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return http.StatusRequestTimeout, "Request body read timed out"
	}
	if errors.Is(err, context.Canceled) || (req != nil && req.Context().Err() != nil) {
		return 499, "Request cancelled"
	}
	return http.StatusBadRequest, "Failed to read request body"
}

func extractMaxBytesError(err error) (*http.MaxBytesError, bool) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return maxErr, true
	}
	return nil, false
}

func formatBodyLimit(limit int64) string {
	const mb = 1024 * 1024
	if limit >= mb {
		return fmt.Sprintf("%dMB", limit/mb)
	}
	return fmt.Sprintf("%dB", limit)
}

func buildBodyTooLargeMessage(limit int64) string {
	return fmt.Sprintf("Request body too large, limit is %s", formatBodyLimit(limit))
}

func readLenientJSONRequestBodyWithPrealloc(req *http.Request, cfg *config.Config) ([]byte, error) {
	return pkghttputil.ReadLenientJSONRequestBodyWithPrealloc(req, gatewayMaxBodySize(cfg))
}

func gatewayMaxBodySize(cfg *config.Config) int64 {
	if cfg == nil {
		return 0
	}
	return cfg.Gateway.MaxBodySize
}
