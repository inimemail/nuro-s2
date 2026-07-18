package handler

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type openAIEdgePromptAuditSettingRepo struct{ values map[string]string }

func (r *openAIEdgePromptAuditSettingRepo) Get(context.Context, string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}
func (r *openAIEdgePromptAuditSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}
func (r *openAIEdgePromptAuditSettingRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}
func (*openAIEdgePromptAuditSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (*openAIEdgePromptAuditSettingRepo) SetMultiple(context.Context, map[string]string) error {
	return nil
}
func (*openAIEdgePromptAuditSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return nil, nil
}
func (*openAIEdgePromptAuditSettingRepo) Delete(context.Context, string) error { return nil }

type openAIEdgePromptAuditEncryptor struct{}

func (openAIEdgePromptAuditEncryptor) Encrypt(value string) (string, error) {
	return base64.RawStdEncoding.EncodeToString([]byte(value)), nil
}
func (openAIEdgePromptAuditEncryptor) Decrypt(value string) (string, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	return string(decoded), err
}

func newEnabledOpenAIEdgePromptAuditService(t *testing.T) *securityaudit.Service {
	t.Helper()
	svc := securityaudit.NewService(
		&openAIEdgePromptAuditSettingRepo{values: map[string]string{}},
		nil,
		nil,
		openAIEdgePromptAuditEncryptor{},
	)
	_, err := svc.SaveConfig(context.Background(), securityaudit.UpdateConfigRequest{
		Enabled: true, WorkerCount: 1, QueueCapacity: 8, AllGroups: true,
		Scanners: []string{"pii"}, RetentionDays: 7, ExpectedVersion: 1,
		Endpoints: []securityaudit.UpdateEndpoint{{
			ID: "guard", Name: "Guard", BaseURL: "https://guard.example", Model: "guard",
			TimeoutMS: 1000, Enabled: true,
		}},
	})
	require.NoError(t, err)
	svc.SetFeatureEnabled(true)
	return svc
}

func TestOpenAIEdgePromptAuditQueuesOnlyAfterComplete(t *testing.T) {
	auditService := newEnabledOpenAIEdgePromptAuditService(t)
	cfg := &config.Config{}
	cfg.Gateway.OpenAIEdgeRS.InternalAPIEnabled = true
	cfg.Gateway.OpenAIEdgeRS.InternalSecret = "edge-secret"
	h := &OpenAIGatewayHandler{
		cfg:                      cfg,
		promptAuditService:       auditService,
		openAIEdgeLeases:         make(map[string]*openAIEdgeLease),
		openAIEdgeLeaseByRequest: make(map[string]string),
	}
	groupID := int64(17)
	apiKey := &service.APIKey{
		ID: 23, Name: "edge-key", GroupID: &groupID,
		Group: &service.Group{ID: groupID, Name: "edge-group", Platform: service.PlatformOpenAI},
		User:  &service.User{ID: 31, Email: "edge@example.com"},
	}
	body := []byte(`{"model":"gpt-5.6-sol","input":"audit after stream"}`)
	prepareContext, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/prepare", `{}`, "edge-secret")
	collector := h.newOpenAIEdgePromptAuditCollector(
		prepareContext,
		apiKey,
		middleware2.AuthSubject{UserID: apiKey.User.ID},
		service.ContentModerationProtocolOpenAIResponses,
		"gpt-5.6-sol",
		body,
		"edge-audit-1",
		"/v1/responses",
	)
	require.NotNil(t, collector)
	require.Equal(t, int64(0), auditService.Runtime().Enqueued, "prepare must not enqueue audit work")

	lease := &openAIEdgeLease{
		edgeRequestID: "edge-audit-1",
		leaseID:       "lease-audit-1",
		apiKey:        apiKey,
		subject:       middleware2.AuthSubject{UserID: apiKey.User.ID},
		promptAudit:   collector,
	}
	h.openAIEdgeLeases[lease.leaseID] = lease
	h.openAIEdgeLeaseByRequest[lease.edgeRequestID] = lease.leaseID
	completeContext, recorder := newOpenAIEdgeTestContext(
		http.MethodPost,
		"/internal/edge/openai/complete",
		`{"edge_request_id":"edge-audit-1","lease_id":"lease-audit-1","success":true,"terminal_event_type":"response.completed"}`,
		"edge-secret",
	)
	h.OpenAIEdgeComplete(completeContext)

	require.Equal(t, http.StatusOK, recorder.Code)
	runtime := auditService.Runtime()
	require.Equal(t, int64(1), runtime.Enqueued)
	require.Equal(t, 1, runtime.QueueLength)
}

func TestOpenAIEdgePromptAuditDisabledIsNoOp(t *testing.T) {
	auditService := securityaudit.NewService(nil, nil, nil, openAIEdgePromptAuditEncryptor{})
	h := &OpenAIGatewayHandler{promptAuditService: auditService}
	c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/prepare", `{}`, "")

	collector := h.newOpenAIEdgePromptAuditCollector(
		c,
		&service.APIKey{},
		middleware2.AuthSubject{},
		service.ContentModerationProtocolOpenAIResponses,
		"gpt-5.6-sol",
		[]byte(`{"input":"disabled"}`),
		"edge-disabled",
		"/v1/responses",
	)

	require.Nil(t, collector)
	require.Equal(t, int64(0), auditService.Runtime().Enqueued)
}

func TestOpenAIEdgeAbortPromptAuditRequiresTerminalRelay(t *testing.T) {
	require.True(t, openAIEdgeAbortShouldFlushPromptAudit(service.OpenAIEdgeAbortRequest{
		Reason: "retry_respond_error", RelayAttempted: true,
	}))
	require.False(t, openAIEdgeAbortShouldFlushPromptAudit(service.OpenAIEdgeAbortRequest{
		Reason: "relay_failed", RelayAttempted: true, FallbackToGo: true,
	}))
	require.False(t, openAIEdgeAbortShouldFlushPromptAudit(service.OpenAIEdgeAbortRequest{
		Reason: "client_disconnect", RelayAttempted: true, ClientDisconnected: true,
	}))
	require.False(t, openAIEdgeAbortShouldFlushPromptAudit(service.OpenAIEdgeAbortRequest{
		Reason: "unsupported_transport",
	}))
	require.True(t, openAIEdgeAbortShouldFlushPromptAudit(service.OpenAIEdgeAbortRequest{
		Reason: "ws_session_dropped", RelayAttempted: true,
	}))
}
