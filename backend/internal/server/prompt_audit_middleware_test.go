package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type promptAuditMiddlewareSettingRepo struct{ values map[string]string }

func (r *promptAuditMiddlewareSettingRepo) Get(context.Context, string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}
func (r *promptAuditMiddlewareSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}
func (r *promptAuditMiddlewareSettingRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}
func (*promptAuditMiddlewareSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (*promptAuditMiddlewareSettingRepo) SetMultiple(context.Context, map[string]string) error {
	return nil
}
func (*promptAuditMiddlewareSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return nil, nil
}
func (*promptAuditMiddlewareSettingRepo) Delete(context.Context, string) error { return nil }

type promptAuditMiddlewareEncryptor struct{}

func (promptAuditMiddlewareEncryptor) Encrypt(value string) (string, error) {
	return base64.RawStdEncoding.EncodeToString([]byte(value)), nil
}
func (promptAuditMiddlewareEncryptor) Decrypt(value string) (string, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	return string(decoded), err
}

func TestPromptAuditCollectorNilServiceIsNoOp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(promptAuditCollectorMiddleware(nil))
	router.GET("/health", func(c *gin.Context) {
		if securityaudit.CollectorFromContext(c.Request.Context()) != nil {
			t.Fatal("nil service unexpectedly attached a collector")
		}
		c.Status(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestPromptAuditCollectorQueuesOnlyAfterHandlerReturns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := securityaudit.NewService(&promptAuditMiddlewareSettingRepo{values: map[string]string{}}, nil, nil, promptAuditMiddlewareEncryptor{})
	_, err := svc.SaveConfig(context.Background(), securityaudit.UpdateConfigRequest{
		Enabled: true, WorkerCount: 1, QueueCapacity: 8, AllGroups: true,
		Scanners: []string{"pii"}, RetentionDays: 7, ExpectedVersion: 1,
		Endpoints: []securityaudit.UpdateEndpoint{{
			ID: "guard", Name: "Guard", BaseURL: "https://guard.example", Model: "guard",
			TimeoutMS: 1000, Enabled: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.Use(promptAuditCollectorMiddleware(svc))
	router.POST("/v1/responses", func(c *gin.Context) {
		collector := securityaudit.CollectorFromContext(c.Request.Context())
		if collector == nil {
			t.Fatal("enabled request missing collector")
		}
		collector.Add(securityaudit.Request{Body: []byte(`{"input":"hello"}`), Protocol: "openai_responses", Stage: "http"})
		if got := svc.Runtime(); got.Enqueued != 0 || got.QueueLength != 0 {
			t.Fatalf("request was queued before handler completion: %+v", got)
		}
		c.Status(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := svc.Runtime(); got.Enqueued != 1 || got.QueueLength != 1 {
		t.Fatalf("request was not queued after handler completion: %+v", got)
	}
}

func TestPromptAuditCollectorDisabledPathHasNoContextValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := securityaudit.NewService(nil, nil, nil, promptAuditMiddlewareEncryptor{})
	router := gin.New()
	router.Use(promptAuditCollectorMiddleware(svc))
	router.GET("/health", func(c *gin.Context) {
		if securityaudit.CollectorFromContext(c.Request.Context()) != nil {
			t.Fatal("disabled request unexpectedly received collector")
		}
		c.Status(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if got := svc.Runtime(); got.Enqueued != 0 || got.Dropped != 0 {
		t.Fatalf("disabled runtime changed: %+v", got)
	}
}

func TestPromptAuditCollectorFlushesWhenHandlerPanics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := securityaudit.NewService(&promptAuditMiddlewareSettingRepo{values: map[string]string{}}, nil, nil, promptAuditMiddlewareEncryptor{})
	_, err := svc.SaveConfig(context.Background(), securityaudit.UpdateConfigRequest{
		Enabled: true, WorkerCount: 1, QueueCapacity: 4, AllGroups: true,
		Scanners: []string{"pii"}, RetentionDays: 7, ExpectedVersion: 1,
		Endpoints: []securityaudit.UpdateEndpoint{{
			ID: "guard", Name: "Guard", BaseURL: "https://guard.example", Model: "guard",
			TimeoutMS: 1000, Enabled: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.Use(promptAuditCollectorMiddleware(svc))
	router.POST("/panic", func(c *gin.Context) {
		collector := securityaudit.CollectorFromContext(c.Request.Context())
		if collector == nil {
			t.Fatal("enabled request missing collector")
		}
		collector.Add(securityaudit.Request{Body: []byte(`{"input":"panic"}`), Protocol: "openai_responses", Stage: "http"})
		panic("intentional test panic")
	})

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected handler panic")
			}
		}()
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/panic", nil))
	}()
	if got := svc.Runtime(); got.Enqueued != 1 || got.QueueLength != 1 {
		t.Fatalf("panic path did not flush collector: %+v", got)
	}
}
