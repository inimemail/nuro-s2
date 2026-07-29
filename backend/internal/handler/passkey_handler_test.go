//go:build unit

package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type passkeySettingRepoStub struct {
	value string
	err   error
}

func (r *passkeySettingRepoStub) Get(context.Context, string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}
func (r *passkeySettingRepoStub) GetValue(context.Context, string) (string, error) {
	return r.value, r.err
}
func (r *passkeySettingRepoStub) Set(context.Context, string, string) error { return nil }
func (r *passkeySettingRepoStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *passkeySettingRepoStub) SetMultiple(context.Context, map[string]string) error { return nil }
func (r *passkeySettingRepoStub) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *passkeySettingRepoStub) Delete(context.Context, string) error { return nil }

func TestBindPasskeyFinishRequestRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/auth/passkey/login/finish",
		strings.NewReader(`{"credential":"`+strings.Repeat("x", passkeyFinishBodyMaxBytes)+`"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")

	_, ok := bindPasskeyFinishRequest(ctx)
	require.False(t, ok)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestPasskeyLoginFailsClosedWhenAdminSwitchIsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := passkeyHandlerTestConfig()
	settings := service.NewSettingService(&passkeySettingRepoStub{value: "false"}, cfg)
	passkeys, err := service.NewPasskeyService(cfg, nil, nil, nil)
	require.NoError(t, err)
	handler := NewPasskeyHandler(passkeys, nil, settings)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/passkey/login/begin", nil)

	handler.BeginLogin(ctx)
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), "PASSKEY_DISABLED")
}

func TestPasskeyLoginDoesNotMaskSettingStoreFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := passkeyHandlerTestConfig()
	settings := service.NewSettingService(
		&passkeySettingRepoStub{err: errors.New("database unavailable")},
		cfg,
	)
	passkeys, err := service.NewPasskeyService(cfg, nil, nil, nil)
	require.NoError(t, err)
	handler := NewPasskeyHandler(passkeys, nil, settings)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/passkey/login/begin", nil)

	handler.BeginLogin(ctx)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "PASSKEY_DISABLED")
}

func passkeyHandlerTestConfig() *config.Config {
	return &config.Config{WebAuthn: config.WebAuthnConfig{
		Enabled: true, RPDisplayName: "Sub2API", RPID: "sub2api.example.com",
		RPOrigins: []string{"https://sub2api.example.com"},
	}}
}
