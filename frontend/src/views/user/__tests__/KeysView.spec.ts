import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import KeysView from '../KeysView.vue'

const { list, stats, showError } = vi.hoisted(() => ({ list: vi.fn(), stats: vi.fn(), showError: vi.fn() }))
vi.mock('@/api', () => ({
  keysAPI: { list }, usageAPI: { getDashboardApiKeysUsage: stats },
  userGroupsAPI: { getAvailable: async () => [], getUserGroupRates: async () => ({}) },
  authAPI: { getPublicSettings: async () => ({}) }
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess: vi.fn() }) }))
vi.mock('@/stores/onboarding', () => ({ useOnboardingStore: () => ({}) }))
vi.mock('@/composables/useLocaleRefresh', () => ({ useLocaleRefresh: vi.fn() }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))
const SlotLayout = defineComponent({ template: '<div><slot/><slot name="filters"/><slot name="actions"/><slot name="table"/><slot name="pagination"/></div>' })
const Table = defineComponent({ props: ['data', 'loading'], template: '<div><div v-if="loading">table-loading</div><div v-for="row in data" :key="row.id">{{row.name}}<slot name="cell-usage" :row="row"/></div></div>' })
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (error: Error) => void; const promise = new Promise<T>((a,b) => { resolve=a; reject=b }); return { promise, resolve, reject } }
const page = (id: number) => ({ items: [{ id, name: `key-${id}`, quota: 0, quota_used: 0 }], total: 2, pages: 2 })
const usage = (id: number, cost: number) => ({ stats: { [id]: { today_actual_cost: cost, total_actual_cost: cost } } })
function mountKeys() { return shallowMount(KeysView, { global: { stubs: { AppLayout: SlotLayout, TablePageLayout: SlotLayout, DataTable: Table } } }) }

describe('KeysView asynchronous usage', () => {
  beforeEach(() => { vi.clearAllMocks(); localStorage.clear(); list.mockResolvedValue(page(1)); stats.mockResolvedValue(usage(1, 1)) })
  it('shows the list before slow stats and discards the previous page response', async () => {
    const slow = deferred<ReturnType<typeof usage>>()
    stats.mockReturnValueOnce(slow.promise).mockResolvedValueOnce(usage(2, 2))
    const wrapper = mountKeys(); await flushPromises()
    expect(wrapper.text()).toContain('key-1'); expect(wrapper.text()).not.toContain('table-loading'); expect(wrapper.text()).toContain('common.loading'); expect(wrapper.text()).not.toContain('$0.0000')
    list.mockResolvedValueOnce(page(2))
    await wrapper.find('button[title="common.refresh"]').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('key-2'); expect(wrapper.text()).toContain('$2.0000')
    slow.resolve(usage(1, 99)); await flushPromises()
    expect(wrapper.text()).toContain('$2.0000'); expect(wrapper.text()).not.toContain('$99.0000'); wrapper.unmount()
  })
  it('offers a retry after stats failure without reloading the list', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    stats.mockRejectedValueOnce(new Error('fixture')).mockResolvedValueOnce(usage(1, 3))
    const wrapper = mountKeys(); await flushPromises()
    const retry = wrapper.findAll('button').find(button => button.text().includes('keys.usageStatsFailed'))
    expect(retry).toBeDefined(); await retry!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('$3.0000'); expect(list).toHaveBeenCalledTimes(1); wrapper.unmount(); vi.restoreAllMocks()
  })
  it('aborts an in-flight stats request on unmount', async () => {
    const slow = deferred<ReturnType<typeof usage>>(); stats.mockReturnValueOnce(slow.promise)
    const wrapper = mountKeys(); await flushPromises(); const signal = stats.mock.calls[0]![1].signal as AbortSignal
    wrapper.unmount(); expect(signal.aborted).toBe(true); slow.resolve(usage(1, 10)); await flushPromises(); expect(showError).not.toHaveBeenCalled()
  })
})
