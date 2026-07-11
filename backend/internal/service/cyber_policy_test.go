package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDetectOpenAICyberPolicy(t *testing.T) {
	payload := []byte(`{"error":{"type":"safety_error","code":"cyber_policy","message":"This request has been flagged for potentially high-risk cyber activity."}}`)
	decision := DetectOpenAICyberPolicy(payload)
	require.True(t, decision.Matched)
	require.Equal(t, "cyber_policy", decision.Code)
	require.Contains(t, decision.Message, "high-risk cyber activity")
}

func TestCyberPolicyAnchorPrefersSessionHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Request.Header.Set("conversation_id", "conv-1")
	c.Request.Header.Set("session_id", "sess-1")

	anchorType, anchorHash := OpenAICyberPolicyAnchor(c, nil)
	require.Equal(t, CyberPolicyAnchorSessionID, anchorType)
	require.NotEmpty(t, anchorHash)
}

func TestCyberPolicySessionBlockRequiresRiskControlEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Request.Header.Set("session_id", "sess-1")
	payload := []byte(`{"error":{"code":"cyber_policy","message":"This request has been flagged for potentially high-risk cyber activity."}}`)
	account := &Account{ID: 7, Platform: PlatformOpenAI}

	disabledSvc := &OpenAIGatewayService{}
	decision := disabledSvc.handleOpenAICyberPolicyEvent(c, account, false, "rid-1", payload, nil)
	require.True(t, decision.Matched)
	require.False(t, decision.SessionBlocked)

	enabledSvc := &OpenAIGatewayService{
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{
			SettingKeyRiskControlEnabled: "true",
		}}, nil),
	}
	decision = enabledSvc.handleOpenAICyberPolicyEvent(c, account, false, "rid-2", payload, nil)
	require.True(t, decision.SessionBlocked)
	_, blocked := enabledSvc.checkOpenAICyberPolicySessionBlock(c.Request.Context(), account, decision.AnchorType, decision.AnchorHash)
	require.True(t, blocked)
}

func TestNonCyberPolicyEventDoesNotReadRiskControlSetting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Request.Header.Set("session_id", "sess-miss")

	repo := &cyberPolicyCountingSettingRepo{values: map[string]string{
		SettingKeyRiskControlEnabled: "true",
	}}
	svc := &OpenAIGatewayService{
		settingService: NewSettingService(repo, nil),
	}

	decision := svc.handleOpenAICyberPolicyEvent(c, &Account{ID: 9, Platform: PlatformOpenAI}, false, "rid-miss", []byte(`{"error":{"code":"server_error","message":"temporary upstream failure"}}`), nil)
	require.False(t, decision.Matched)
	require.Equal(t, int64(0), repo.getValueCalls.Load())

	decision = svc.handleOpenAICyberPolicyEvent(c, &Account{ID: 9, Platform: PlatformOpenAI}, false, "rid-hit", []byte(`{"error":{"code":"cyber_policy","message":"This request has been flagged for potentially high-risk cyber activity."}}`), nil)
	require.True(t, decision.SessionBlocked)
	require.Equal(t, int64(1), repo.getValueCalls.Load())
}

func TestCyberPolicyHandleEventSetsBlockWhenEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Request.Header.Set("session_id", "sess-42")
	svc := &OpenAIGatewayService{
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{
			SettingKeyRiskControlEnabled: "true",
		}}, nil),
	}
	account := &Account{ID: 42, Platform: PlatformOpenAI}
	payload := []byte(`{"error":{"code":"cyber_policy","message":"This request has been flagged for potentially high-risk cyber activity."}}`)
	decision := svc.handleOpenAICyberPolicyEvent(c, account, false, "rid-3", payload, nil)
	require.True(t, decision.Matched)
	require.True(t, decision.SessionBlocked)
	_, blocked := svc.checkOpenAICyberPolicySessionBlock(c.Request.Context(), account, decision.AnchorType, decision.AnchorHash)
	require.True(t, blocked)
}

func TestOpenAIHealthProbeCyberPolicyDoesNotSetSessionBlock(t *testing.T) {
	c, _ := configuredHealthProbeContext(t)
	c.Request.Header.Set("session_id", "health-probe-session")
	svc := &OpenAIGatewayService{
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{
			SettingKeyRiskControlEnabled: "true",
		}}, nil),
	}
	account := &Account{ID: 43, Platform: PlatformOpenAI}
	payload := []byte(`{"error":{"code":"cyber_policy","message":"This request has been flagged for potentially high-risk cyber activity."}}`)

	decision := svc.handleOpenAICyberPolicyEvent(c, account, false, "rid-health-probe", payload, nil)

	require.True(t, decision.Matched)
	require.False(t, decision.SessionBlocked)
	_, blocked := svc.checkOpenAICyberPolicySessionBlock(c.Request.Context(), account, decision.AnchorType, decision.AnchorHash)
	require.False(t, blocked)
}

type cyberPolicyCountingSettingRepo struct {
	values        map[string]string
	getValueCalls atomic.Int64
}

func (s *cyberPolicyCountingSettingRepo) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *cyberPolicyCountingSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	s.getValueCalls.Add(1)
	if v, ok := s.values[key]; ok {
		return v, nil
	}
	return "", ErrSettingNotFound
}

func (s *cyberPolicyCountingSettingRepo) Set(ctx context.Context, key, value string) error {
	panic("unexpected Set call")
}

func (s *cyberPolicyCountingSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *cyberPolicyCountingSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	panic("unexpected SetMultiple call")
}

func (s *cyberPolicyCountingSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (s *cyberPolicyCountingSettingRepo) Delete(ctx context.Context, key string) error {
	panic("unexpected Delete call")
}
