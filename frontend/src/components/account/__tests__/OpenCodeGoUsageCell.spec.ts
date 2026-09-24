import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { ref } from 'vue'
import type { Account } from '@/types'
import type { OpenCodeUsageState } from '@/api/admin/opencodeUsage'
import OpenCodeGoUsageCell from '../OpenCodeGoUsageCell.vue'

const api = vi.hoisted(() => ({ get: vi.fn(), refresh: vi.fn(), configure: vi.fn() }))
vi.mock('@/api/admin/opencodeUsage', async importOriginal => ({ ...await importOriginal<object>(), openCodeUsageAPI: api }))
vi.mock('vue-i18n', async importOriginal => ({ ...await importOriginal<object>(), useI18n: () => ({ t: (key: string) => key, locale: ref('en') }) }))
const account = (id: number) => ({ id, platform: 'opencode_go', type: 'apikey', extra: { cn_billing_mode: 'coding_plan' }, credentials: {} }) as Account
const state = (id: number, percent: number): OpenCodeUsageState => ({ account_id: id, eligible: true, auto_refresh: false, global_enabled: false, source: 'configured', snapshot: { last_attempt_at: 1, next_refresh_at: 2, tiers: [{ window: '5h', used_percent: percent }] } })
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(r => { resolve = r }); return { promise, resolve } }

describe('OpenCode usage account identity', () => {
  it('ignores a manual refresh that finishes after switching accounts', async () => {
    api.get.mockImplementation((id: number) => Promise.resolve(state(id, id * 10)))
    const old = deferred<OpenCodeUsageState>()
    api.refresh.mockReturnValue(old.promise)
    const wrapper = mount(OpenCodeGoUsageCell, { props: { account: account(1) } })
    await flushPromises()
    await wrapper.get('button[aria-label="openCodeUsage.refresh"]').trigger('click')
    await wrapper.setProps({ account: account(2) })
    await flushPromises()
    expect(wrapper.text()).toContain('20%')
    old.resolve(state(1, 99))
    await flushPromises()
    expect(wrapper.text()).toContain('20%')
    expect(wrapper.text()).not.toContain('99%')
    wrapper.unmount()
  })
  it('loads saved usage without sending an upstream refresh on mount', async () => {
    api.refresh.mockClear()
    api.get.mockResolvedValue(state(1, 35))
    const wrapper = mount(OpenCodeGoUsageCell, { props: { account: account(1) } })
    await flushPromises()
    expect(api.refresh).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('35%')
    expect(wrapper.text()).toContain('openCodeUsage.globalOff')
    wrapper.unmount()
  })
})
