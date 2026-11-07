package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Separate windows cannot be reconstructed from the per-model minimum fraction.
type QuotaSummaryBucket struct {
	BucketID          string   `json:"bucketId"`
	DisplayName       string   `json:"displayName,omitempty"`
	Window            string   `json:"window"`
	ResetTime         string   `json:"resetTime,omitempty"`
	RemainingFraction *float64 `json:"remainingFraction"`
}
type QuotaSummaryGroup struct {
	DisplayName string               `json:"displayName"`
	Description string               `json:"description,omitempty"`
	Buckets     []QuotaSummaryBucket `json:"buckets"`
}
type QuotaSummaryResponse struct {
	Groups []QuotaSummaryGroup `json:"groups"`
}
type QuotaSummaryHTTPError struct{ StatusCode int }

func (e *QuotaSummaryHTTPError) Error() string {
	return fmt.Sprintf("quota summary HTTP %d", e.StatusCode)
}

// This read-only endpoint is used only by explicit/background account usage
// refresh. Its caller supplies the existing shared deadline, not a new budget.
func (c *Client) RetrieveUserQuotaSummary(ctx context.Context, accessToken, projectID string) (*QuotaSummaryResponse, error) {
	if c == nil || c.httpClient == nil {
		return nil, fmt.Errorf("antigravity client is not configured")
	}
	body, err := json.Marshal(FetchAvailableModelsRequest{Project: projectID})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i, baseURL := range BaseURLs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := NewAPIRequestWithURL(ctx, baseURL, "retrieveUserQuotaSummary", accessToken, body)
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if i+1 < len(BaseURLs) && shouldFallbackToNextURL(err, 0) {
				continue
			}
			return nil, err
		}
		const limit = 1 << 20
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read quota summary: %w", readErr)
		}
		if len(data) > limit {
			return nil, fmt.Errorf("quota summary exceeds response limit")
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = &QuotaSummaryHTTPError{StatusCode: resp.StatusCode}
			if i+1 < len(BaseURLs) && shouldFallbackToNextURL(nil, resp.StatusCode) {
				continue
			}
			return nil, lastErr
		}
		var out QuotaSummaryResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("decode quota summary: %w", err)
		}
		return &out, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no quota summary endpoint configured")
	}
	return nil, lastErr
}
