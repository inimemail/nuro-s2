package securityaudit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var defaultScanners = []string{
	"violent", "non_violent_illegal_acts", "sexual_content_or_sexual_acts",
	"pii", "suicide_and_self_harm", "unethical_acts",
	"politically_sensitive_topics", "copyright_violation", "jailbreak",
}

func DefaultConfig() Config {
	return Config{
		Mode: ModeOff, WorkerCount: DefaultWorkerCount, QueueCapacity: DefaultQueueCapacity,
		AllGroups: true, Scanners: append([]string(nil), defaultScanners...),
		RetentionDays: DefaultRetentionDays, Version: 1,
	}
}

func normalizeConfig(cfg *Config) {
	if cfg.Mode == "" {
		cfg.Mode = ModeOff
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = DefaultWorkerCount
	}
	if cfg.QueueCapacity <= 0 {
		cfg.QueueCapacity = DefaultQueueCapacity
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = DefaultRetentionDays
	}
	if cfg.Version <= 0 {
		cfg.Version = 1
	}
	if len(cfg.Scanners) == 0 {
		cfg.Scanners = append([]string(nil), defaultScanners...)
	}
	cfg.GroupIDs = canonicalInt64s(cfg.GroupIDs)
	cfg.Scanners = canonicalStrings(cfg.Scanners)
	for i := range cfg.Endpoints {
		ep := &cfg.Endpoints[i]
		ep.ID = strings.TrimSpace(ep.ID)
		ep.Name = strings.TrimSpace(ep.Name)
		ep.BaseURL = strings.TrimSpace(ep.BaseURL)
		ep.Model = strings.TrimSpace(ep.Model)
		if ep.TimeoutMS <= 0 {
			ep.TimeoutMS = DefaultTimeoutMS
		}
		ep.AllowedCIDRs = canonicalStrings(ep.AllowedCIDRs)
	}
}

func validateConfig(cfg Config) error {
	if cfg.Mode != ModeOff && cfg.Mode != ModeAsync {
		return infraerrors.BadRequest("PROMPT_AUDIT_MODE_INVALID", "prompt audit supports off or async_audit mode")
	}
	if cfg.Enabled && cfg.Mode != ModeAsync {
		return infraerrors.BadRequest("PROMPT_AUDIT_MODE_REQUIRED", "enabled prompt audit requires async_audit mode")
	}
	if cfg.WorkerCount < 1 || cfg.WorkerCount > 16 {
		return infraerrors.BadRequest("PROMPT_AUDIT_WORKERS_INVALID", "worker_count must be between 1 and 16")
	}
	if cfg.QueueCapacity < 1 || cfg.QueueCapacity > 100000 {
		return infraerrors.BadRequest("PROMPT_AUDIT_QUEUE_INVALID", "queue_capacity must be between 1 and 100000")
	}
	if cfg.RetentionDays < 1 || cfg.RetentionDays > MaxRetentionDays {
		return infraerrors.BadRequest("PROMPT_AUDIT_RETENTION_INVALID", "retention_days is outside the allowed range")
	}
	if len(cfg.Endpoints) > 16 {
		return infraerrors.BadRequest("PROMPT_AUDIT_ENDPOINTS_TOO_MANY", "at most 16 audit endpoints are allowed")
	}
	if len(cfg.GroupIDs) > 10000 {
		return infraerrors.BadRequest("PROMPT_AUDIT_GROUPS_TOO_MANY", "too many audit groups are configured")
	}
	knownScannerSet := make(map[string]struct{}, len(defaultScanners))
	for _, scanner := range defaultScanners {
		knownScannerSet[scanner] = struct{}{}
	}
	for _, scanner := range cfg.Scanners {
		if _, ok := knownScannerSet[normalizeCategory(scanner)]; !ok {
			return infraerrors.BadRequest("PROMPT_AUDIT_SCANNER_INVALID", "prompt audit scanner is invalid")
		}
	}
	if !cfg.AllGroups && len(cfg.GroupIDs) == 0 {
		return infraerrors.BadRequest("PROMPT_AUDIT_GROUPS_REQUIRED", "at least one group is required")
	}
	seen := make(map[string]struct{}, len(cfg.Endpoints))
	enabled := 0
	for _, ep := range cfg.Endpoints {
		if ep.ID == "" || ep.Name == "" || ep.Model == "" {
			return infraerrors.BadRequest("PROMPT_AUDIT_ENDPOINT_INVALID", "endpoint id, name and model are required")
		}
		if len([]rune(ep.ID)) > 64 || len([]rune(ep.Name)) > 128 || len([]rune(ep.Model)) > 255 || len(ep.BaseURL) > 2048 {
			return infraerrors.BadRequest("PROMPT_AUDIT_ENDPOINT_INVALID", "audit endpoint fields are too long")
		}
		if _, ok := seen[ep.ID]; ok {
			return infraerrors.BadRequest("PROMPT_AUDIT_ENDPOINT_DUPLICATE", "endpoint id must be unique")
		}
		seen[ep.ID] = struct{}{}
		if ep.TimeoutMS < 100 || ep.TimeoutMS > MaxTimeoutMS {
			return infraerrors.BadRequest("PROMPT_AUDIT_TIMEOUT_INVALID", "endpoint timeout is outside the allowed range")
		}
		if _, err := normalizeEndpointURL(ep); err != nil {
			return err
		}
		if ep.AllowPrivate && len(ep.AllowedCIDRs) == 0 {
			return infraerrors.BadRequest("PROMPT_AUDIT_PRIVATE_ALLOWLIST_REQUIRED", "private endpoints require an explicit CIDR allowlist")
		}
		if err := validateAllowedCIDRs(ep.AllowedCIDRs); err != nil {
			return err
		}
		if ep.Enabled {
			enabled++
		}
	}
	if cfg.Enabled && enabled == 0 {
		return infraerrors.BadRequest("PROMPT_AUDIT_ENDPOINT_REQUIRED", "at least one enabled endpoint is required")
	}
	return nil
}

func (s *Service) loadConfig(ctx context.Context) error {
	cfg, err := s.readConfig(ctx)
	if err != nil {
		return err
	}
	s.storeConfig(cfg)
	return nil
}

func (s *Service) readConfig(ctx context.Context) (Config, error) {
	cfg := DefaultConfig()
	if s == nil || s.settingRepo == nil {
		return cfg, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyPromptAuditConfig)
	if err != nil {
		if errors.Is(err, service.ErrSettingNotFound) {
			return cfg, nil
		}
		return Config{}, err
	}
	return decodeConfig(raw)
}

func decodeConfig(raw string) (Config, error) {
	cfg := DefaultConfig()
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return Config{}, infraerrors.BadRequest("PROMPT_AUDIT_CONFIG_INVALID", "stored prompt audit config is invalid").WithCause(err)
		}
	}
	normalizeConfig(&cfg)
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (s *Service) refreshConfig(ctx context.Context) error {
	if s == nil || s.settingRepo == nil {
		return nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyPromptAuditConfig)
	if err != nil {
		if errors.Is(err, service.ErrSettingNotFound) {
			return nil
		}
		return err
	}
	candidate, err := decodeConfig(raw)
	if err != nil {
		return err
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	current := s.configSnapshot()
	if candidate.Version < current.Version ||
		(candidate.Version == current.Version && !candidate.UpdatedAt.After(current.UpdatedAt)) {
		return nil
	}
	s.storeConfig(candidate)
	if lastError, _ := s.lastError.Load().(string); lastError == "config_load_failed" || lastError == "config_refresh_failed" {
		s.lastError.Store("")
	}
	return nil
}

func (s *Service) PublicConfig() PublicConfig {
	cfg := s.configSnapshot()
	result := PublicConfig{
		Enabled: cfg.Enabled, Mode: cfg.Mode, WorkerCount: cfg.WorkerCount,
		QueueCapacity: cfg.QueueCapacity, AllGroups: cfg.AllGroups,
		GroupIDs: append([]int64(nil), cfg.GroupIDs...), Scanners: append([]string(nil), cfg.Scanners...),
		StorePass: cfg.StorePass, RetentionDays: cfg.RetentionDays, Version: cfg.Version,
		UpdatedAt: cfg.UpdatedAt,
	}
	result.Endpoints = make([]PublicEndpoint, 0, len(cfg.Endpoints))
	for _, ep := range cfg.Endpoints {
		hasToken := strings.TrimSpace(ep.TokenCiphertext) != ""
		status := "not_configured"
		if hasToken {
			status = "configured"
		}
		result.Endpoints = append(result.Endpoints, PublicEndpoint{
			ID: ep.ID, Name: ep.Name, BaseURL: ep.BaseURL, Model: ep.Model,
			TimeoutMS: ep.TimeoutMS, Enabled: ep.Enabled, HasToken: hasToken,
			TokenStatus: status, AllowPrivate: ep.AllowPrivate,
			AllowedCIDRs: append([]string(nil), ep.AllowedCIDRs...),
		})
	}
	return result
}

func (s *Service) SaveConfig(ctx context.Context, req UpdateConfigRequest) (PublicConfig, error) {
	if s == nil || s.encryptor == nil || (s.settingRepo == nil && s.db == nil) {
		return PublicConfig{}, infraerrors.New(503, "PROMPT_AUDIT_UNAVAILABLE", "prompt audit service is unavailable")
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	var (
		cfg Config
		err error
	)
	if s.db != nil {
		cfg, err = s.saveConfigDB(ctx, req)
	} else {
		current, readErr := s.readConfig(ctx)
		if readErr != nil {
			return PublicConfig{}, infraerrors.New(500, "PROMPT_AUDIT_CONFIG_LOAD_FAILED", "failed to load prompt audit config").WithCause(readErr)
		}
		cfg, err = s.buildConfig(req, current)
		if err == nil {
			var raw []byte
			raw, err = json.Marshal(cfg)
			if err == nil {
				err = s.settingRepo.Set(ctx, SettingKeyPromptAuditConfig, string(raw))
				if err != nil {
					err = infraerrors.New(500, "PROMPT_AUDIT_CONFIG_SAVE_FAILED", "failed to save prompt audit config").WithCause(err)
				}
			} else {
				err = infraerrors.New(500, "PROMPT_AUDIT_CONFIG_ENCODE_FAILED", "failed to encode prompt audit config").WithCause(err)
			}
		}
	}
	if err != nil {
		return PublicConfig{}, err
	}
	s.storeConfig(cfg)
	return s.PublicConfig(), nil
}

func (s *Service) buildConfig(req UpdateConfigRequest, current Config) (Config, error) {
	if req.ExpectedVersion > 0 && req.ExpectedVersion != current.Version {
		return Config{}, infraerrors.Conflict("PROMPT_AUDIT_CONFIG_CONFLICT", "prompt audit config changed; reload and retry")
	}
	cfg := Config{
		Enabled: req.Enabled, Mode: ModeOff, WorkerCount: req.WorkerCount,
		QueueCapacity: req.QueueCapacity, AllGroups: req.AllGroups,
		GroupIDs: append([]int64(nil), req.GroupIDs...), Scanners: append([]string(nil), req.Scanners...),
		StorePass: req.StorePass, RetentionDays: req.RetentionDays,
		Version: current.Version + 1, UpdatedAt: time.Now().UTC(), HashSecret: current.HashSecret,
	}
	if strings.TrimSpace(cfg.HashSecret) == "" {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return Config{}, infraerrors.New(500, "PROMPT_AUDIT_HASH_SECRET_FAILED", "failed to protect prompt audit hashes")
		}
		ciphertext, err := s.encryptor.Encrypt(hex.EncodeToString(secret))
		if err != nil {
			return Config{}, infraerrors.New(500, "PROMPT_AUDIT_HASH_SECRET_FAILED", "failed to protect prompt audit hashes")
		}
		cfg.HashSecret = ciphertext
	}
	if cfg.Enabled {
		cfg.Mode = ModeAsync
	}
	currentEndpoints := make(map[string]EndpointConfig, len(current.Endpoints))
	for _, ep := range current.Endpoints {
		currentEndpoints[ep.ID] = ep
	}
	for _, input := range req.Endpoints {
		ep := EndpointConfig{
			ID: strings.TrimSpace(input.ID), Name: strings.TrimSpace(input.Name),
			BaseURL: strings.TrimSpace(input.BaseURL), Model: strings.TrimSpace(input.Model),
			TimeoutMS: input.TimeoutMS, Enabled: input.Enabled, AllowPrivate: input.AllowPrivate,
			AllowedCIDRs: append([]string(nil), input.AllowedCIDRs...),
		}
		switch {
		case input.ClearToken:
		case strings.TrimSpace(input.Token) != "":
			ciphertext, err := s.encryptor.Encrypt(strings.TrimSpace(input.Token))
			if err != nil {
				return Config{}, infraerrors.New(500, "PROMPT_AUDIT_TOKEN_ENCRYPT_FAILED", "failed to protect endpoint token")
			}
			ep.TokenCiphertext = ciphertext
		default:
			if configured, ok := currentEndpoints[ep.ID]; ok && endpointCredentialBindingEqual(configured, ep) {
				ep.TokenCiphertext = configured.TokenCiphertext
			}
		}
		cfg.Endpoints = append(cfg.Endpoints, ep)
	}
	normalizeConfig(&cfg)
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (s *Service) saveConfigDB(ctx context.Context, req UpdateConfigRequest) (Config, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Config{}, infraerrors.New(500, "PROMPT_AUDIT_CONFIG_SAVE_FAILED", "failed to save prompt audit config").WithCause(err)
	}
	defer func() { _ = tx.Rollback() }()
	defaultRaw, err := json.Marshal(DefaultConfig())
	if err != nil {
		return Config{}, infraerrors.New(500, "PROMPT_AUDIT_CONFIG_ENCODE_FAILED", "failed to encode prompt audit config")
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (key) DO NOTHING`, SettingKeyPromptAuditConfig, string(defaultRaw)); err != nil {
		return Config{}, infraerrors.New(500, "PROMPT_AUDIT_CONFIG_SAVE_FAILED", "failed to save prompt audit config").WithCause(err)
	}
	var currentRaw string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", SettingKeyPromptAuditConfig).Scan(&currentRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Config{}, infraerrors.New(500, "PROMPT_AUDIT_CONFIG_SAVE_FAILED", "failed to save prompt audit config")
		}
		return Config{}, infraerrors.New(500, "PROMPT_AUDIT_CONFIG_SAVE_FAILED", "failed to save prompt audit config").WithCause(err)
	}
	current, err := decodeConfig(currentRaw)
	if err != nil {
		return Config{}, err
	}
	cfg, err := s.buildConfig(req, current)
	if err != nil {
		return Config{}, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return Config{}, infraerrors.New(500, "PROMPT_AUDIT_CONFIG_ENCODE_FAILED", "failed to encode prompt audit config")
	}
	result, err := tx.ExecContext(ctx, "UPDATE settings SET value=$2, updated_at=NOW() WHERE key=$1", SettingKeyPromptAuditConfig, string(raw))
	if err != nil {
		return Config{}, infraerrors.New(500, "PROMPT_AUDIT_CONFIG_SAVE_FAILED", "failed to save prompt audit config").WithCause(err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Config{}, infraerrors.Conflict("PROMPT_AUDIT_CONFIG_CONFLICT", "prompt audit config changed; reload and retry")
	}
	if err := tx.Commit(); err != nil {
		return Config{}, infraerrors.New(500, "PROMPT_AUDIT_CONFIG_SAVE_FAILED", "failed to save prompt audit config").WithCause(err)
	}
	return cfg, nil
}

func (cfg Config) includesGroup(groupID *int64) bool {
	if cfg.AllGroups {
		return true
	}
	if groupID == nil {
		return false
	}
	i := sort.Search(len(cfg.GroupIDs), func(i int) bool { return cfg.GroupIDs[i] >= *groupID })
	return i < len(cfg.GroupIDs) && cfg.GroupIDs[i] == *groupID
}

func canonicalInt64s(values []int64) []int64 {
	set := make(map[int64]struct{}, len(values))
	for _, value := range values {
		if value > 0 {
			set[value] = struct{}{}
		}
	}
	result := make([]int64, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func canonicalStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
