package securityaudit

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	SettingKeyPromptAuditConfig = "prompt_audit_config"
	ModeOff                     = "off"
	ModeAsync                   = "async_audit"
	DefaultWorkerCount          = 2
	DefaultQueueCapacity        = 2048
	DefaultRetentionDays        = 7
	MaxRetentionDays            = 90
	DefaultTimeoutMS            = 3000
	MaxTimeoutMS                = 30000
	MaxPromptRunes              = 12000
	MaxPreviewRunes             = 240
	MaxEventPageSize            = 100
	MaxEventSearchRunes         = 256
	MaxAuditBodyBytes           = 256 << 10
	MaxQueuedBytes              = 32 << 20
)

type Mode string

type EndpointConfig struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	BaseURL         string   `json:"base_url"`
	Model           string   `json:"model"`
	TokenCiphertext string   `json:"token_ciphertext,omitempty"`
	TimeoutMS       int      `json:"timeout_ms"`
	Enabled         bool     `json:"enabled"`
	AllowPrivate    bool     `json:"allow_private"`
	AllowedCIDRs    []string `json:"allowed_cidrs,omitempty"`
}

type Config struct {
	Enabled       bool             `json:"enabled"`
	Mode          Mode             `json:"mode"`
	WorkerCount   int              `json:"worker_count"`
	QueueCapacity int              `json:"queue_capacity"`
	AllGroups     bool             `json:"all_groups"`
	GroupIDs      []int64          `json:"group_ids"`
	Scanners      []string         `json:"scanners"`
	Endpoints     []EndpointConfig `json:"endpoints"`
	StorePass     bool             `json:"store_pass_events"`
	RetentionDays int              `json:"retention_days"`
	Version       int64            `json:"version"`
	UpdatedAt     time.Time        `json:"updated_at"`
	HashSecret    string           `json:"hash_secret_ciphertext,omitempty"`
}

type PublicEndpoint struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	BaseURL      string   `json:"base_url"`
	Model        string   `json:"model"`
	TimeoutMS    int      `json:"timeout_ms"`
	Enabled      bool     `json:"enabled"`
	HasToken     bool     `json:"has_token"`
	TokenStatus  string   `json:"token_status"`
	AllowPrivate bool     `json:"allow_private"`
	AllowedCIDRs []string `json:"allowed_cidrs,omitempty"`
}

type PublicConfig struct {
	Enabled       bool             `json:"enabled"`
	Mode          Mode             `json:"mode"`
	WorkerCount   int              `json:"worker_count"`
	QueueCapacity int              `json:"queue_capacity"`
	AllGroups     bool             `json:"all_groups"`
	GroupIDs      []int64          `json:"group_ids"`
	Scanners      []string         `json:"scanners"`
	Endpoints     []PublicEndpoint `json:"endpoints"`
	StorePass     bool             `json:"store_pass_events"`
	RetentionDays int              `json:"retention_days"`
	Version       int64            `json:"version"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

type UpdateEndpoint struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	BaseURL      string   `json:"base_url"`
	Model        string   `json:"model"`
	Token        string   `json:"token,omitempty"`
	ClearToken   bool     `json:"clear_token"`
	TimeoutMS    int      `json:"timeout_ms"`
	Enabled      bool     `json:"enabled"`
	AllowPrivate bool     `json:"allow_private"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
}

type UpdateConfigRequest struct {
	Enabled         bool             `json:"enabled"`
	WorkerCount     int              `json:"worker_count"`
	QueueCapacity   int              `json:"queue_capacity"`
	AllGroups       bool             `json:"all_groups"`
	GroupIDs        []int64          `json:"group_ids"`
	Scanners        []string         `json:"scanners"`
	Endpoints       []UpdateEndpoint `json:"endpoints"`
	StorePass       bool             `json:"store_pass_events"`
	RetentionDays   int              `json:"retention_days"`
	ExpectedVersion int64            `json:"expected_version"`
}

type Request struct {
	RequestID    string
	UserID       int64
	Username     string
	UserEmail    string
	APIKeyID     int64
	APIKeyName   string
	GroupID      *int64
	GroupName    string
	Provider     string
	Endpoint     string
	Protocol     string
	Model        string
	Body         []byte
	PromptTexts  []string
	CaptureError string
	Stage        string
}

func (r Request) cloneBody() Request {
	if len(r.Body) > 0 {
		r.Body = append([]byte(nil), r.Body...)
	}
	if len(r.PromptTexts) > 0 {
		r.PromptTexts = append([]string(nil), r.PromptTexts...)
		for i := range r.PromptTexts {
			r.PromptTexts[i] = strings.Clone(r.PromptTexts[i])
		}
	}
	if r.GroupID != nil {
		id := *r.GroupID
		r.GroupID = &id
	}
	return r
}

type Event struct {
	ID             int64     `json:"id"`
	RequestID      string    `json:"request_id"`
	UserID         int64     `json:"user_id"`
	UserEmail      string    `json:"user_email"`
	APIKeyID       int64     `json:"api_key_id"`
	APIKeyName     string    `json:"api_key_name"`
	GroupID        *int64    `json:"group_id,omitempty"`
	GroupName      string    `json:"group_name"`
	Provider       string    `json:"provider"`
	Endpoint       string    `json:"endpoint"`
	Protocol       string    `json:"protocol"`
	Model          string    `json:"model"`
	PromptHash     string    `json:"prompt_hash"`
	Preview        string    `json:"redacted_preview"`
	PromptLength   int       `json:"prompt_length"`
	MessageCount   int       `json:"message_count"`
	Stage          string    `json:"stage"`
	Decision       string    `json:"decision"`
	RiskLevel      string    `json:"risk_level"`
	Action         string    `json:"action"`
	Categories     []string  `json:"categories"`
	ScannerBackend string    `json:"scanner_backend"`
	ScannerVersion string    `json:"scanner_version"`
	EndpointID     string    `json:"guard_endpoint_id"`
	LatencyMS      int       `json:"latency_ms"`
	ErrorCode      string    `json:"error_code,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type Runtime struct {
	Mode          Mode      `json:"mode"`
	WorkerCount   int       `json:"worker_count"`
	QueueCapacity int       `json:"queue_capacity"`
	QueueLength   int       `json:"queue_length"`
	Enqueued      int64     `json:"enqueued"`
	Dropped       int64     `json:"dropped"`
	Processed     int64     `json:"processed"`
	Failed        int64     `json:"failed"`
	LastError     string    `json:"last_error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type EventFilter struct {
	Page      int    `json:"page"`
	PageSize  int    `json:"page_size"`
	Decision  string `json:"decision"`
	RiskLevel string `json:"risk_level"`
	GroupID   *int64 `json:"group_id,omitempty"`
	UserID    *int64 `json:"user_id,omitempty"`
	Search    string `json:"search"`
}

type EventList struct {
	Items    []Event `json:"items"`
	Total    int64   `json:"total"`
	Page     int     `json:"page"`
	PageSize int     `json:"page_size"`
}

type DeletePreview struct {
	Count     int64     `json:"count"`
	MaxID     int64     `json:"max_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Token     string    `json:"confirmation_token"`
}

// Collector is request-local. It only stores immutable metadata references
// until the response has finished; it never performs I/O on the request path.
type Collector struct {
	mu       sync.Mutex
	requests []Request
	wsTurns  int
	owner    *Service
}

// FlushNextWSTurn is called by the existing post-turn hook, after the
// downstream turn has completed. It keeps long-lived WebSocket sessions from
// retaining every prior payload without adding work to the turn's TTFT path.
func (c *Collector) FlushNextWSTurn() {
	if c == nil || c.owner == nil {
		return
	}
	c.mu.Lock()
	index := -1
	for i := range c.requests {
		if c.requests[i].Stage == "first_turn" || c.requests[i].Stage == "subsequent_turn" {
			index = i
			break
		}
	}
	if index < 0 {
		c.mu.Unlock()
		return
	}
	req := c.requests[index]
	copy(c.requests[index:], c.requests[index+1:])
	c.requests = c.requests[:len(c.requests)-1]
	c.mu.Unlock()
	c.owner.flushRequests([]Request{req})
}

func (c *Collector) Add(req Request) {
	if c == nil || (len(req.Body) == 0 && len(req.PromptTexts) == 0 && req.CaptureError == "") {
		return
	}
	c.mu.Lock()
	if len(c.requests) < 16 {
		if len(req.Body) > MaxAuditBodyBytes {
			req.Body = nil
			req.CaptureError = "prompt_body_too_large"
		}
		if len(req.PromptTexts) > 0 {
			textBytes := 0
			for _, value := range req.PromptTexts {
				textBytes += len(value)
				if textBytes > MaxAuditBodyBytes {
					req.PromptTexts = nil
					req.CaptureError = "prompt_body_too_large"
					break
				}
			}
		}
		if req.Stage == "ws_turn" {
			if c.wsTurns == 0 {
				req.Stage = "first_turn"
			} else {
				req.Stage = "subsequent_turn"
			}
			c.wsTurns++
		}
		c.requests = append(c.requests, req)
	}
	c.mu.Unlock()
}

func (c *Collector) take() []Request {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	items := c.requests
	c.requests = nil
	return items
}

type collectorContextKey struct{}

func WithCollector(ctx context.Context, collector *Collector) context.Context {
	return context.WithValue(ctx, collectorContextKey{}, collector)
}

func CollectorFromContext(ctx context.Context) *Collector {
	if ctx == nil {
		return nil
	}
	collector, _ := ctx.Value(collectorContextKey{}).(*Collector)
	return collector
}

func RequestFromContentInput(input service.ContentModerationCheckInput, stage string) Request {
	return Request{
		RequestID: input.RequestID, UserID: input.UserID, UserEmail: input.UserEmail,
		APIKeyID: input.APIKeyID, APIKeyName: input.APIKeyName, GroupID: input.GroupID,
		GroupName: input.GroupName, Provider: input.Provider, Endpoint: input.Endpoint,
		Protocol: input.Protocol, Model: input.Model, Body: input.Body, Stage: stage,
	}
}
