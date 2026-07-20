import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import UpstreamBillingRateCell from '../UpstreamBillingRateCell.vue'
import type { Account } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${JSON.stringify(params)}` : key
    })
  }
})

const now = Date.parse('2026-07-17T08:00:10Z')

function makeAccount(options: {
  observed?: number
  limit?: number
  autoProbe?: boolean
  guardEnabled?: boolean
  status?: 'ok' | 'failed' | 'unsupported'
  groupCount?: number
} = {}): Account {
  const {
    observed,
    limit = 1,
    autoProbe = true,
    guardEnabled = true,
    status = 'ok',
    groupCount = 1
  } = options
  const groups = Array.from({ length: groupCount }, (_, index) => ({
    id: index + 10,
    name: `Group ${index + 1}`,
    upstream_billing_guard_max_multiplier: limit
  }))

  return {
    id: 1,
    name: 'OpenAI Key',
    platform: 'openai',
    type: 'apikey',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: false,
    created_at: '2026-07-17T00:00:00Z',
    updated_at: '2026-07-17T00:00:00Z',
    schedulable: true,
    upstream_billing_guard_enabled: guardEnabled,
    upstream_billing_guard_observed_multiplier: observed ?? null,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    openai_pool_soft_cooldown_until: null,
    openai_pool_soft_cooldown_due: false,
    openai_pool_recovery_probe_in_flight: false,
    groups: groups as any,
    account_groups: groups.map((group, index) => ({
      account_id: 1,
      group_id: group.id,
      priority: index + 1,
      upstream_billing_guard_max_multiplier: null,
      created_at: '2026-07-17T00:00:00Z',
      group: group as any
    })),
    extra: {
      upstream_billing_probe_enabled: autoProbe,
      upstream_billing_probe: {
        status,
        ...(observed === undefined ? {} : { data: { effective_rate_multiplier: observed } }),
        received_at: '2026-07-17T08:00:00Z',
        fresh_until: '2026-07-17T08:01:00Z',
        last_attempt_at: '2026-07-17T08:00:00Z',
        next_probe_at: '2026-07-17T08:00:30Z'
      }
    }
  } as Account
}

function mountCell(account: Account, globalProbeEnabled = true) {
  return mount(UpstreamBillingRateCell, {
    props: { account, now, globalProbeEnabled },
    global: { stubs: { Icon: true } }
  })
}

describe('UpstreamBillingRateCell', () => {
  it('shows one upstream multiplier and marks an equal threshold as available', () => {
    const wrapper = mountCell(makeAccount({ observed: 1, limit: 1 }))

    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('1x')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').attributes('data-guard-state')).toBe('available')
    expect(wrapper.text()).not.toContain('1x / 1x')
  })

  it('marks a group yellow when the observed multiplier exceeds its limit', () => {
    const wrapper = mountCell(makeAccount({ observed: 1.01, limit: 1 }))

    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').attributes('data-guard-state')).toBe('blocked')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10-details"]').text()).toContain(
      'admin.accounts.upstreamBilling.guardPaused'
    )
  })

  it('marks configured groups yellow when account automatic probing is disabled', () => {
    const wrapper = mountCell(makeAccount({ observed: 0.5, limit: 1, autoProbe: false }))

    expect(wrapper.get('[data-testid="upstream-billing-status"]').text()).toBe(
      'admin.accounts.upstreamBilling.autoProbeDisabled'
    )
    expect(wrapper.text()).not.toContain('common.time.')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').attributes('data-guard-state')).toBe('blocked')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10-details"]').text()).toContain(
      'admin.accounts.upstreamBilling.guardProbeDisabled'
    )
  })

  it('shows configured groups as disabled when the account master switch is off', () => {
    const wrapper = mountCell(makeAccount({ observed: 2, limit: 1, guardEnabled: false }))

    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').attributes('data-guard-state')).toBe('disabled')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10-details"]').text()).toContain(
      'admin.accounts.upstreamBilling.guardDisabled'
    )
  })

  it('treats an explicit group null as unrestricted over a stale binding value', () => {
    const account = makeAccount({ observed: 2, limit: 1 })
    const group = { ...(account.groups?.[0] as any), upstream_billing_guard_max_multiplier: null }
    account.groups = [group]
    account.account_groups = [{
      ...(account.account_groups?.[0] as any),
      upstream_billing_guard_max_multiplier: 1.5,
      group
    }]

    const wrapper = mountCell(account)

    expect(wrapper.find('[data-testid="upstream-billing-guard-group-10"]').exists()).toBe(false)
  })

  it('uses the refreshed group default when the binding has no explicit override', () => {
    const account = makeAccount({ observed: 1.5, limit: 2 })
    account.groups = [{ ...(account.groups?.[0] as any), upstream_billing_guard_max_multiplier: 2 }]
    account.account_groups = [{
      ...(account.account_groups?.[0] as any),
      // This is the stale effective value returned before the group update.
      upstream_billing_guard_max_multiplier: 1,
      upstream_billing_guard_override_max_multiplier: null,
      group: account.groups[0]
    }]

    const wrapper = mountCell(account)

    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').attributes('data-guard-state')).toBe('available')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10-details"]').text()).toContain(
      'admin.accounts.upstreamBilling.guardLimitDetail:{"rate":"2x"}'
    )
  })

  it('uses a richer account group object when the binding group is shallow', () => {
    const account = makeAccount({ observed: 0.8, limit: 1 })
    account.account_groups = [{
      ...(account.account_groups?.[0] as any),
      upstream_billing_guard_max_multiplier: null,
      group: { id: 10, name: 'Shallow group' } as any
    }]

    const wrapper = mountCell(account)

    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').text()).toContain('Group 1')
  })

  it('marks groups gray while waiting for the first successful probe', () => {
    const wrapper = mountCell(makeAccount({ limit: 1 }))

    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').attributes('data-guard-state')).toBe('pending')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10-details"]').text()).toContain(
      'admin.accounts.upstreamBilling.guardWaitingFirstProbe'
    )
  })

  it('uses the backend scheduling observation when a stale snapshot still contains a multiplier', () => {
    const account = makeAccount({ observed: 2, limit: 1 })
    account.upstream_billing_guard_observed_multiplier = null
    const wrapper = mountCell(account)

    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('2x')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').attributes('data-guard-state')).toBe('pending')
  })

  it('keeps using the last successful multiplier after a probe failure', () => {
    const wrapper = mountCell(makeAccount({ observed: 1.2, limit: 1, status: 'failed' }))

    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('1.2x')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').attributes('data-guard-state')).toBe('blocked')
    expect(wrapper.get('[data-testid="upstream-billing-status"]').text()).toContain(
      'admin.accounts.upstreamBilling.failedWithLast'
    )
  })

  it('includes group, limit, update time, and state in hover details', () => {
    const wrapper = mountCell(makeAccount({ observed: 0.8, limit: 1.5 }))
    const details = wrapper.get('[data-testid="upstream-billing-guard-group-10-details"]').text()

    expect(details).toContain('Group 1')
    expect(details).toContain('admin.accounts.upstreamBilling.currentRateDetail:{"rate":"0.8x"}')
    expect(details).toContain('admin.accounts.upstreamBilling.guardLimitDetail:{"rate":"1.5x"}')
    expect(details).toContain('common.time.justNow')
    expect(details).toContain('admin.accounts.upstreamBilling.guardAvailable')
  })

  it('keeps additional protected groups accessible from the compact overflow tooltip', () => {
    const wrapper = mountCell(makeAccount({ observed: 0.8, limit: 1, groupCount: 5 }))

    const details = wrapper.get('[data-testid="upstream-billing-hidden-group-details"]').text()
    expect(details).toContain('Group 4')
    expect(details).toContain('Group 5')
    expect(details).toContain('admin.accounts.upstreamBilling.currentRateDetail:{"rate":"0.8x"}')
    expect(details).toContain('admin.accounts.upstreamBilling.guardAvailable')
  })

  it('formats old probe timestamps as readable hours instead of raw seconds', () => {
    const account = makeAccount({ observed: 0.8, limit: 1 })
    const snapshot = account.extra?.upstream_billing_probe
    if (snapshot) snapshot.received_at = '2026-07-17T00:00:00Z'
    const wrapper = mountCell(account)

    expect(wrapper.get('[data-testid="upstream-billing-status"]').text()).toContain('common.time.hoursAgo:{"n":8}')
    expect(wrapper.text()).not.toContain('28810')
  })

  it('shows the global probe disabled state instead of historical age', () => {
    const wrapper = mountCell(makeAccount({ observed: 0.8, limit: 1 }), false)

    expect(wrapper.get('[data-testid="upstream-billing-status"]').text()).toBe(
      'admin.accounts.upstreamBilling.globalProbeDisabled'
    )
    expect(wrapper.text()).not.toContain('common.time.')
    expect(wrapper.text()).not.toContain('秒前更新')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10"]').attributes('data-guard-state')).toBe('pending')
    expect(wrapper.get('[data-testid="upstream-billing-guard-group-10-details"]').text()).toContain(
      'admin.accounts.upstreamBilling.guardGlobalProbeDisabled'
    )
  })
})
