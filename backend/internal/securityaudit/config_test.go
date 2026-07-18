package securityaudit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type auditSettingRepo struct {
	mu     sync.Mutex
	values map[string]string
	getErr error
}

func (r *auditSettingRepo) Get(context.Context, string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}
func (r *auditSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return "", r.getErr
	}
	value, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}
func (r *auditSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}
func (r *auditSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (r *auditSettingRepo) SetMultiple(context.Context, map[string]string) error { return nil }
func (r *auditSettingRepo) GetAll(context.Context) (map[string]string, error)    { return nil, nil }
func (r *auditSettingRepo) Delete(context.Context, string) error                 { return nil }

type auditEncryptor struct{}

func (auditEncryptor) Encrypt(value string) (string, error) {
	return "protected:" + base64.RawStdEncoding.EncodeToString([]byte(value)), nil
}
func (auditEncryptor) Decrypt(value string) (string, error) {
	const prefix = "protected:"
	if len(value) < len(prefix) || value[:len(prefix)] != prefix {
		return "", errors.New("invalid ciphertext")
	}
	plain, err := base64.RawStdEncoding.DecodeString(value[len(prefix):])
	return string(plain), err
}

type countingAuditEncryptor struct {
	decrypts int
}

func (e *countingAuditEncryptor) Encrypt(value string) (string, error) {
	return auditEncryptor{}.Encrypt(value)
}

func (e *countingAuditEncryptor) Decrypt(value string) (string, error) {
	e.decrypts++
	return auditEncryptor{}.Decrypt(value)
}

func TestSaveConfigPreservesAndClearsEndpointToken(t *testing.T) {
	repo := &auditSettingRepo{values: map[string]string{}}
	svc := NewService(repo, nil, nil, auditEncryptor{})

	base := UpdateConfigRequest{
		Enabled: true, WorkerCount: 2, QueueCapacity: 20, AllGroups: true,
		Scanners: []string{"pii", "violent"}, RetentionDays: 7, ExpectedVersion: 1,
		Endpoints: []UpdateEndpoint{{ID: "primary", Name: "Primary", BaseURL: "https://guard.example/v1", Model: "guard", Token: "secret", TimeoutMS: 2000, Enabled: true}},
	}
	created, err := svc.SaveConfig(context.Background(), base)
	if err != nil || created.Version != 2 || !created.Endpoints[0].HasToken {
		t.Fatalf("created=%+v err=%v", created, err)
	}

	base.ExpectedVersion = created.Version
	base.Endpoints[0].Token = ""
	base.Endpoints[0].BaseURL = "https://GUARD.example/v1/"
	preserved, err := svc.SaveConfig(context.Background(), base)
	if err != nil || !preserved.Endpoints[0].HasToken {
		t.Fatalf("preserved=%+v err=%v", preserved, err)
	}

	base.ExpectedVersion = preserved.Version
	base.Endpoints[0].BaseURL = "https://other.example/v1"
	unbound, err := svc.SaveConfig(context.Background(), base)
	if err != nil || unbound.Endpoints[0].HasToken {
		t.Fatalf("changed endpoint retained token: config=%+v err=%v", unbound, err)
	}

	base.ExpectedVersion = unbound.Version
	base.Endpoints[0].Token = "replacement"
	rebound, err := svc.SaveConfig(context.Background(), base)
	if err != nil || !rebound.Endpoints[0].HasToken {
		t.Fatalf("replacement token was not configured: config=%+v err=%v", rebound, err)
	}

	base.ExpectedVersion = rebound.Version
	base.Endpoints[0].Token = ""
	base.Endpoints[0].ClearToken = true
	cleared, err := svc.SaveConfig(context.Background(), base)
	if err != nil || cleared.Endpoints[0].HasToken {
		t.Fatalf("cleared=%+v err=%v", cleared, err)
	}
	if _, err := svc.SaveConfig(context.Background(), base); err == nil {
		t.Fatal("expected optimistic version conflict")
	}
}

func TestSavedConfigKeepsPromptHMACStableAcrossServiceInstances(t *testing.T) {
	repo := &auditSettingRepo{values: map[string]string{}}
	first := NewService(repo, nil, nil, auditEncryptor{})
	_, err := first.SaveConfig(context.Background(), UpdateConfigRequest{
		Enabled: true, WorkerCount: 1, QueueCapacity: 8, AllGroups: true,
		Scanners: defaultScanners, RetentionDays: 7, ExpectedVersion: 1,
		Endpoints: []UpdateEndpoint{{ID: "guard", Name: "Guard", BaseURL: "https://guard.example", Model: "qwen3guard", TimeoutMS: 1000, Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := first.promptHash("stable prompt")
	if want == "" {
		t.Fatal("saved config did not install a prompt HMAC key")
	}

	second := NewService(repo, nil, nil, auditEncryptor{})
	if err := second.loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := second.promptHash("stable prompt"); got != want {
		t.Fatalf("prompt HMAC changed across instances: got=%q want=%q", got, want)
	}
	publicRaw, err := json.Marshal(second.PublicConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicRaw), "hash_secret") {
		t.Fatalf("public config exposed internal hash material: %s", publicRaw)
	}
}

func TestProbeReusesStoredTokenOnlyForSameEndpointBinding(t *testing.T) {
	encryptor := &countingAuditEncryptor{}
	svc := NewService(nil, nil, nil, encryptor)
	ciphertext, err := encryptor.Encrypt("stored-token")
	if err != nil {
		t.Fatal(err)
	}
	cfg := enabledAuditTestConfig("https://guard.example/v1")
	cfg.Endpoints[0].TokenCiphertext = ciphertext
	svc.storeConfig(cfg)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = svc.Probe(canceled, UpdateEndpoint{
		ID: "guard", Name: "Guard", BaseURL: "https://GUARD.example/v1/", Model: "qwen3guard", TimeoutMS: 1000,
	})
	if err != nil || encryptor.decrypts != 1 {
		t.Fatalf("same binding did not reuse stored token: decrypts=%d err=%v", encryptor.decrypts, err)
	}

	_, err = svc.Probe(canceled, UpdateEndpoint{
		ID: "guard", Name: "Guard", BaseURL: "https://other.example/v1", Model: "qwen3guard", TimeoutMS: 1000,
	})
	if err != nil || encryptor.decrypts != 1 {
		t.Fatalf("changed binding reused stored token: decrypts=%d err=%v", encryptor.decrypts, err)
	}
}

func TestRefreshConfigAdvancesButNeverRegressesTrustedSnapshot(t *testing.T) {
	repo := &auditSettingRepo{values: map[string]string{}}
	svc := NewService(repo, nil, nil, auditEncryptor{})
	newer := enabledAuditTestConfig("https://guard.example")
	newer.Version = 5
	newer.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(newer)
	if err != nil {
		t.Fatal(err)
	}
	repo.values[SettingKeyPromptAuditConfig] = string(raw)
	if err := svc.refreshConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := svc.configSnapshot(); got.Version != 5 || !got.Enabled {
		t.Fatalf("new config not applied: %+v", got)
	}

	stale := newer
	stale.Version = 4
	stale.Enabled = false
	stale.Mode = ModeOff
	stale.UpdatedAt = newer.UpdatedAt.Add(time.Hour)
	raw, err = json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	repo.values[SettingKeyPromptAuditConfig] = string(raw)
	repo.mu.Unlock()
	if err := svc.refreshConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := svc.configSnapshot(); got.Version != 5 || !got.Enabled {
		t.Fatalf("trusted config regressed: %+v", got)
	}

	repo.mu.Lock()
	repo.getErr = errors.New("temporary read failure")
	repo.mu.Unlock()
	if err := svc.refreshConfig(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}
	if got := svc.configSnapshot(); got.Version != 5 || !got.Enabled {
		t.Fatalf("refresh error replaced trusted config: %+v", got)
	}
}

func TestSystemFeatureGateDefaultsOffAndRefreshesIndependently(t *testing.T) {
	cfg := enabledAuditTestConfig("https://guard.example")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	repo := &auditSettingRepo{values: map[string]string{
		SettingKeyPromptAuditConfig: string(raw),
	}}
	svc := NewService(repo, nil, nil, auditEncryptor{})

	if err := svc.loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if svc.FeatureEnabledFast() || svc.EnabledFast() || svc.NewCollector() != nil {
		t.Fatal("missing system feature setting must fail closed")
	}

	repo.mu.Lock()
	repo.values[service.SettingKeyPromptAuditEnabled] = "true"
	repo.mu.Unlock()
	if err := svc.refreshConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !svc.FeatureEnabledFast() || !svc.EnabledFast() || svc.NewCollector() == nil {
		t.Fatal("enabled system feature setting did not activate the existing audit config")
	}

	repo.mu.Lock()
	repo.values[service.SettingKeyPromptAuditEnabled] = "false"
	repo.mu.Unlock()
	if err := svc.refreshConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if svc.FeatureEnabledFast() || svc.EnabledFast() || svc.NewCollector() != nil {
		t.Fatal("disabled system feature setting did not stop collection")
	}
}

func TestSaveConfigUsesDatabaseVersionForCrossInstanceConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stored := enabledAuditTestConfig("https://guard.example")
	stored.Version = 4
	stored.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO settings").WithArgs(SettingKeyPromptAuditConfig, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT value FROM settings WHERE key=\\$1 FOR UPDATE").WithArgs(SettingKeyPromptAuditConfig).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(raw)))
	mock.ExpectRollback()

	svc := NewService(nil, db, nil, auditEncryptor{})
	_, err = svc.SaveConfig(context.Background(), UpdateConfigRequest{ExpectedVersion: 1})
	if err == nil {
		t.Fatal("stale process snapshot overwrote the database config")
	}
	if got := svc.configSnapshot().Version; got != 1 {
		t.Fatalf("conflicted config changed local snapshot to version %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveConfigDatabaseCASCommitsNextVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stored := DefaultConfig()
	stored.Version = 3
	stored.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO settings").WithArgs(SettingKeyPromptAuditConfig, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT value FROM settings WHERE key=\\$1 FOR UPDATE").WithArgs(SettingKeyPromptAuditConfig).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(raw)))
	mock.ExpectExec("UPDATE settings SET value=\\$2").WithArgs(SettingKeyPromptAuditConfig, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	svc := NewService(nil, db, nil, auditEncryptor{})
	updated, err := svc.SaveConfig(context.Background(), UpdateConfigRequest{
		Enabled: true, WorkerCount: 2, QueueCapacity: 20, AllGroups: true,
		Scanners: defaultScanners, RetentionDays: 7, ExpectedVersion: 3,
		Endpoints: []UpdateEndpoint{{ID: "guard", Name: "Guard", BaseURL: "https://guard.example", Model: "qwen3guard", TimeoutMS: 1000, Enabled: true}},
	})
	if err != nil || updated.Version != 4 {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateConfigRequiresExplicitPrivateAllowlist(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled, cfg.Mode = true, ModeAsync
	cfg.Endpoints = []EndpointConfig{{ID: "local", Name: "Local", BaseURL: "http://127.0.0.1:9000", Model: "guard", Enabled: true, AllowPrivate: true, TimeoutMS: 1000}}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected private allowlist validation error")
	}
	cfg.Endpoints[0].AllowedCIDRs = []string{"127.0.0.1/32"}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("explicit private endpoint rejected: %v", err)
	}
}
