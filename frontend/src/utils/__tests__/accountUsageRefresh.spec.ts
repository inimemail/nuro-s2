import { describe, expect, it } from 'vitest'
import { buildOpenAIUsageRefreshKey, buildUpstreamBillingGuardRefreshKey } from '../accountUsageRefresh'

describe('buildOpenAIUsageRefreshKey', () => {
  it('会在 codex 快照变化时生成不同 key', () => {
    const base = {
      id: 1,
      platform: 'openai',
      type: 'oauth',
      updated_at: '2026-03-07T10:00:00Z',
      last_used_at: '2026-03-07T09:59:00Z',
      extra: {
        codex_usage_updated_at: '2026-03-07T10:00:00Z',
        codex_5h_used_percent: 0,
        codex_7d_used_percent: 0
      }
    } as any

    const next = {
      ...base,
      extra: {
        ...base.extra,
        codex_usage_updated_at: '2026-03-07T10:01:00Z',
        codex_5h_used_percent: 100
      }
    }

    expect(buildOpenAIUsageRefreshKey(base)).not.toBe(buildOpenAIUsageRefreshKey(next))
  })

  it('会在 last_used_at 变化时生成不同 key', () => {
    const base = {
      id: 3,
      platform: 'openai',
      type: 'oauth',
      updated_at: '2026-03-07T10:00:00Z',
      last_used_at: '2026-03-07T10:00:00Z',
      extra: {
        codex_usage_updated_at: '2026-03-07T10:00:00Z',
        codex_5h_used_percent: 12,
        codex_7d_used_percent: 24
      }
    } as any

    const next = {
      ...base,
      last_used_at: '2026-03-07T10:02:00Z'
    }

    expect(buildOpenAIUsageRefreshKey(base)).not.toBe(buildOpenAIUsageRefreshKey(next))
  })

  it('非 OpenAI OAuth 账号返回空 key', () => {
    expect(buildOpenAIUsageRefreshKey({
      id: 2,
      platform: 'anthropic',
      type: 'oauth',
      updated_at: '2026-03-07T10:00:00Z',
      last_used_at: '2026-03-07T10:00:00Z',
      extra: {}
    } as any)).toBe('')
  })
})

describe('buildUpstreamBillingGuardRefreshKey', () => {
  it('changes when a bound group threshold changes', () => {
    const base = {
      upstream_billing_guard_enabled: true,
      upstream_billing_guard_observed_multiplier: 1,
      account_groups: [{ group_id: 10, upstream_billing_guard_max_multiplier: 1, group: { id: 10 } }],
      extra: { upstream_billing_probe_enabled: true }
    } as any
    const next = {
      ...base,
      account_groups: [{ ...base.account_groups[0], upstream_billing_guard_max_multiplier: 2 }]
    }

    expect(buildUpstreamBillingGuardRefreshKey(base)).not.toBe(buildUpstreamBillingGuardRefreshKey(next))
  })

  it('treats an explicit group null as a policy change', () => {
    const base = {
      upstream_billing_guard_enabled: true,
      upstream_billing_guard_observed_multiplier: 1,
      account_groups: [{ group_id: 10, upstream_billing_guard_max_multiplier: 1, group: { id: 10, upstream_billing_guard_max_multiplier: 1 } }],
      extra: { upstream_billing_probe_enabled: true }
    } as any
    const next = {
      ...base,
      account_groups: [{ ...base.account_groups[0], group: { id: 10, upstream_billing_guard_max_multiplier: null } }]
    }

    expect(buildUpstreamBillingGuardRefreshKey(base)).not.toBe(buildUpstreamBillingGuardRefreshKey(next))
  })
})
