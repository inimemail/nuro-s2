import type { Account } from '@/types'

const normalizeUsageRefreshValue = (value: unknown): string => {
  if (value == null) return ''
  return String(value)
}

export const buildOpenAIUsageRefreshKey = (account: Pick<Account, 'id' | 'platform' | 'type' | 'updated_at' | 'last_used_at' | 'rate_limit_reset_at' | 'extra'>): string => {
  if (account.platform !== 'openai' || account.type !== 'oauth') {
    return ''
  }

  const extra = account.extra ?? {}
  return [
    account.id,
    account.updated_at,
    account.last_used_at,
    account.rate_limit_reset_at,
    extra.codex_auto_reset_mode,
    extra.codex_reset_credits,
    extra.codex_reset_credits_supported,
    extra.codex_reset_credits_updated_at,
    extra.codex_usage_updated_at,
    extra.codex_5h_used_percent,
    extra.codex_5h_reset_at,
    extra.codex_5h_reset_after_seconds,
    extra.codex_5h_window_minutes,
    extra.codex_7d_used_percent,
    extra.codex_7d_reset_at,
    extra.codex_7d_reset_after_seconds,
    extra.codex_7d_window_minutes
  ].map(normalizeUsageRefreshValue).join('|')
}

export const buildUpstreamBillingGuardRefreshKey = (
  account: Pick<Account, 'upstream_billing_guard_enabled' | 'upstream_billing_guard_observed_multiplier' | 'account_groups' | 'extra'>
): string => {
  const bindings = (account.account_groups ?? [])
    .map((binding) => {
      const group = binding.group
      const hasGroupLimit = group != null && Object.prototype.hasOwnProperty.call(group, 'upstream_billing_guard_max_multiplier')
      const groupLimit = hasGroupLimit ? group?.upstream_billing_guard_max_multiplier : ''
      return [binding.group_id, binding.upstream_billing_guard_max_multiplier, groupLimit].map(normalizeUsageRefreshValue).join(':')
    })
    .sort()
  return [
    account.upstream_billing_guard_enabled,
    account.upstream_billing_guard_observed_multiplier,
    account.extra?.upstream_billing_probe_enabled,
    ...bindings
  ].map(normalizeUsageRefreshValue).join('|')
}
