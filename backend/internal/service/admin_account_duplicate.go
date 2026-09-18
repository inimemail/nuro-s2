package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Optional interfaces keep existing admin/repository adapters compatible.
type AdminAccountDuplicateService interface {
	DuplicateAccount(context.Context, int64, string, string) (*Account, error)
	RecoverDuplicateAccount(context.Context, int64, string, string) (*Account, error)
}

type AccountDuplicateRepository interface {
	CreateWithAccountGroups(context.Context, *Account, []AccountGroup) error
}

const duplicateAccountOperationIDExtraKey = "duplicate_operation_id"

func duplicateAccountName(source string) string {
	const suffix = " (Copy)"
	name := strings.TrimSpace(source)
	// The local Ent schema uses MaxLen (bytes), not MaxRuneLen. Preserve
	// complete UTF-8 characters while leaving space for the suffix.
	for len(name) > 100-len(suffix) {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name + suffix
}

func cloneAccountJSONMap(value map[string]any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(data, &result)
	return result, err
}

// Keep administrator settings, including local billing/cache/first-token
// options. Observations and ownership markers belong to the source only.
func duplicateAccountExtra(value map[string]any) (map[string]any, error) {
	extra, err := cloneAccountJSONMap(value)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{
		duplicateAccountOperationIDExtraKey, codexFingerprintSeedExtraKey,
		"crs_account_id", "crs_kind", "crs_synced_at",
		"quota_used", "quota_daily_used", "quota_weekly_used", "quota_daily_start", "quota_weekly_start", "quota_daily_reset_at", "quota_weekly_reset_at",
		"model_rate_limits", "session_window_utilization", "passive_usage_7d_utilization", "passive_usage_7d_reset", "passive_usage_7d_oi_utilization", "passive_usage_7d_oi_reset", "passive_usage_sampled_at",
		GrokQuotaSnapshotExtraKey, GrokBillingSnapshotExtraKey, UpstreamBillingProbeExtraKey, OllamaCloudUsageSnapshotExtraKey,
		"openai_responses_supported", "openai_compact_supported", "openai_compact_checked_at", "openai_compact_last_status", "openai_compact_last_error",
		"antigravity_credits_overages", "antigravity_force_token_refresh", "antigravity_force_token_refresh_at", "antigravity_force_token_refresh_reason",
		"drive_storage_limit", "drive_storage_usage", "drive_tier_updated_at",
		"codex_primary_used_percent", "codex_primary_reset_after_seconds", "codex_primary_window_minutes", "codex_primary_reset_at",
		"codex_secondary_used_percent", "codex_secondary_reset_after_seconds", "codex_secondary_window_minutes", "codex_secondary_reset_at",
		"codex_primary_over_secondary_percent", "codex_usage_updated_at",
		"codex_5h_used_percent", "codex_5h_reset_after_seconds", "codex_5h_window_minutes", "codex_5h_reset_at",
		"codex_7d_used_percent", "codex_7d_reset_after_seconds", "codex_7d_window_minutes", "codex_7d_reset_at",
	} {
		delete(extra, key)
	}
	for _, provider := range []string{PlatformKimi, PlatformZhipu, PlatformDeepSeek, PlatformMiniMax, PlatformOpenCodeGo} {
		for _, suffix := range []string{cnExtraSuffix5hUsed, cnExtraSuffix5hReset, cnExtraSuffixWeeklyUsed, cnExtraSuffixWeeklyReset, cnExtraSuffixUsageUpdated, "monthly_used_percent", "monthly_reset_at"} {
			delete(extra, cnExtraKey(provider, suffix))
		}
	}
	return extra, nil
}

func duplicateAccountOperationID(id int64, actor, key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("admin.accounts.duplicate\x00%s\x00%d\x00%s", strings.TrimSpace(actor), id, strings.TrimSpace(key)))))
}

func (s *adminServiceImpl) RecoverDuplicateAccount(ctx context.Context, id int64, actor, key string) (*Account, error) {
	operationID := duplicateAccountOperationID(id, actor, key)
	if operationID == "" {
		return nil, nil
	}
	accounts, err := s.accountRepo.FindByExtraField(ctx, duplicateAccountOperationIDExtraKey, operationID)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, nil
	}
	return &accounts[0], nil
}

func (s *adminServiceImpl) DuplicateAccount(ctx context.Context, id int64, actor, key string) (*Account, error) {
	if existing, err := s.RecoverDuplicateAccount(ctx, id, actor, key); err != nil || existing != nil {
		return existing, err
	}
	source, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if source.IsCredentialShadow() {
		return nil, infraerrors.BadRequest("ACCOUNT_DUPLICATE_SHADOW_UNSUPPORTED", "linked credential shadow accounts cannot be duplicated")
	}
	switch source.Type {
	case AccountTypeAPIKey, AccountTypeUpstream, AccountTypeBedrock, AccountTypeServiceAccount:
	default:
		return nil, infraerrors.BadRequest("ACCOUNT_DUPLICATE_CREDENTIAL_TYPE_UNSUPPORTED", "accounts with rotating or unsupported credential types cannot be duplicated")
	}
	repo, ok := s.accountRepo.(AccountDuplicateRepository)
	if !ok {
		return nil, errors.New("account duplicate repository is not configured")
	}
	credentials, err := cloneAccountJSONMap(source.Credentials)
	if err != nil {
		return nil, fmt.Errorf("clone credentials: %w", err)
	}
	extra, err := duplicateAccountExtra(source.Extra)
	if err != nil {
		return nil, fmt.Errorf("clone extra: %w", err)
	}
	if err := NormalizeHeaderOverrideCredentials(credentials); err != nil {
		return nil, err
	}
	if source.Platform == PlatformOpenCodeGo {
		if err := NormalizeOpenCodeProtocolRulesCredentials(credentials); err != nil {
			return nil, infraerrors.BadRequest("INVALID_OPENCODE_PROTOCOL_RULES", err.Error())
		}
	}
	if err := ValidateQuotaResetConfig(extra); err != nil {
		return nil, err
	}
	ComputeQuotaResetAt(extra)
	if operationID := duplicateAccountOperationID(id, actor, key); operationID != "" {
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[duplicateAccountOperationIDExtraKey] = operationID
	}
	proxyID := source.ProxyID
	if source.ProxyFallbackOriginID != nil {
		proxyID = source.ProxyFallbackOriginID
	}
	// Explicitly copy configuration, never the runtime/scheduling state.
	copy := &Account{
		Name: duplicateAccountName(source.Name), Notes: cloneGroupPointer(source.Notes),
		Platform: source.Platform, Type: source.Type, Credentials: credentials, Extra: extra,
		ProxyID: cloneGroupPointer(proxyID), Concurrency: source.Concurrency, Priority: source.Priority,
		RateMultiplier: cloneGroupPointer(source.RateMultiplier), LoadFactor: cloneGroupPointer(source.LoadFactor),
		ExpiresAt: cloneGroupPointer(source.ExpiresAt), AutoPauseOnExpired: source.AutoPauseOnExpired,
		Status: StatusActive, Schedulable: false,
		UpstreamBillingGuardEnabled:       source.UpstreamBillingGuardEnabled,
		UpstreamBillingGuardMaxMultiplier: source.UpstreamBillingGuardMaxMultiplier,
	}
	groups := make([]AccountGroup, 0, len(source.AccountGroups))
	for _, binding := range source.AccountGroups {
		groups = append(groups, AccountGroup{
			GroupID: binding.GroupID, Priority: binding.Priority,
			UpstreamBillingGuardOverrideMaxMultiplier: cloneGroupPointer(binding.UpstreamBillingGuardOverrideMaxMultiplier),
			UpstreamBillingGuardOverrideMinMultiplier: cloneGroupPointer(binding.UpstreamBillingGuardOverrideMinMultiplier),
		})
	}
	if len(groups) == 0 {
		for i, groupID := range source.GroupIDs {
			groups = append(groups, AccountGroup{GroupID: groupID, Priority: i + 1})
		}
	}
	// Group validity and the copy itself are checked/committed in one transaction.
	if err := repo.CreateWithAccountGroups(ctx, copy, groups); err != nil {
		return nil, fmt.Errorf("create duplicate account: %w", err)
	}
	return copy, nil
}
