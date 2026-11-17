import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { Account, OllamaCloudUsageState } from '@/types'
import CNProviderUsageCell from '../CNProviderUsageCell.vue'
import OllamaCloudUsageCell from '../OllamaCloudUsageCell.vue'
import QuotaUsageBar from '../QuotaUsageBar.vue'
const api = vi.hoisted(() => ({ getCNProviderQuota: vi.fn(), getCNProviderBalance: vi.fn(), getOllamaCloudUsage: vi.fn(), refreshOllamaCloudUsage: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: api } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const account = (id: number, extra: Record<string, unknown> = {}) => ({ id, platform: 'kimi', type: 'apikey', extra: { cn_billing_mode: 'coding_plan', ...extra }, credentials: {} }) as Account
const ollama = (id: number) => ({ ...account(id), platform: 'openai', credentials: { base_url: 'https://ollama.com/v1' } }) as Account
const quota = (percent: number) => ({ provider: 'kimi', success: true, tiers: [{ window: 'monthly', used_percent: percent }] })
const state = (id: number, percent: number): OllamaCloudUsageState => ({ account_id: id, eligible: true, configured: true, auto_refresh_enabled: false, encryption_key_configured: true, snapshot: { status: 'ok', last_attempt_at: '', next_refresh_at: '', data: { five_hour: { used_percent: percent } } } })
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (error: unknown) => void; const promise = new Promise<T>((r, j) => { resolve = r; reject = j }); return { promise, resolve, reject } }
beforeEach(() => { vi.resetAllMocks() })
describe('quota snapshots and request ownership', () => {
  it('distinguishes missing values from zero and renders Kimi monthly', () => {
    api.getCNProviderQuota.mockReturnValue(new Promise(() => {}))
    const wrapper = mount(CNProviderUsageCell, { props: { account: account(1, { kimi_5h_used_percent: null, kimi_weekly_used_percent: '', kimi_monthly_used_percent: 0 }) } })
    expect(wrapper.text()).toContain('1mo')
    expect(wrapper.findAll('[role="progressbar"]')).toHaveLength(1)
    expect(wrapper.text()).toContain('0%')
    wrapper.unmount()
  })
  it('ignores an old result and finally after an account switch', async () => {
    const first = deferred<ReturnType<typeof quota>>(), second = deferred<ReturnType<typeof quota>>()
    api.getCNProviderQuota.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const wrapper = mount(CNProviderUsageCell, { props: { account: account(1) } })
    const signal = api.getCNProviderQuota.mock.calls[0]?.[1].signal as AbortSignal
    await wrapper.setProps({ account: account(2) })
    expect(signal.aborted).toBe(true)
    first.resolve(quota(99))
    await flushPromises()
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).not.toContain('99%')
    second.resolve(quota(20))
    await flushPromises()
    expect(wrapper.text()).toContain('20%')
    wrapper.unmount()
  })
  it('updates a saved snapshot without probing again or accepting an older result', async () => {
    const pending = deferred<ReturnType<typeof quota>>()
    api.getCNProviderQuota.mockReturnValue(pending.promise)
    const wrapper = mount(CNProviderUsageCell, { props: { account: account(1) } })
    await wrapper.setProps({ account: account(1, { kimi_monthly_used_percent: 75 }) })
    pending.resolve(quota(10))
    await flushPromises()
    expect(wrapper.text()).toContain('75%')
    expect(api.getCNProviderQuota).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })
  it('keeps the new identity request alive when its saved snapshot also changes', async () => {
    const first = deferred<ReturnType<typeof quota>>(), second = deferred<ReturnType<typeof quota>>()
    api.getCNProviderQuota.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const wrapper = mount(CNProviderUsageCell, { props: { account: account(1, { kimi_monthly_used_percent: 15 }) } })
    await wrapper.setProps({ account: account(2, { kimi_monthly_used_percent: 35 }) })
    const signal = api.getCNProviderQuota.mock.calls[1]?.[1].signal as AbortSignal
    expect(signal.aborted).toBe(false)
    expect(wrapper.text()).toContain('35%')
    first.resolve(quota(99))
    second.resolve(quota(50))
    await flushPromises()
    expect(wrapper.text()).toContain('50%')
    expect(wrapper.text()).not.toContain('99%')
    expect(api.getCNProviderQuota).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })
  it('retains last-good data and sanitizes a failed manual refresh', async () => {
    api.getCNProviderQuota.mockResolvedValueOnce(quota(42)).mockResolvedValueOnce({ success: false, error: '<html>secret</html>' })
    const wrapper = mount(CNProviderUsageCell, { props: { account: account(1) } })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('42%')
    expect(wrapper.text()).toContain('quotaDisplay.failed')
    expect(wrapper.html()).not.toContain('secret')
    wrapper.unmount()
  })
  it('does not probe ineligible accounts', () => {
    const wrapper = mount(CNProviderUsageCell, { props: { account: ollama(1) } })
    expect(api.getCNProviderQuota).not.toHaveBeenCalled()
    expect(wrapper.text()).toBe('')
    wrapper.unmount()
  })
  it.each(['kimi', 'zhipu', 'minimax'] as const)('gives explicit payg precedence over legacy coding for %s', async platform => {
    api.getCNProviderBalance.mockResolvedValue({ provider: platform, success: true, balance: 12 })
    const wrapper = mount(CNProviderUsageCell, { props: { account: { ...account(1, { cn_billing_mode: ' payg ' }), platform, credentials: { account_mode: 'coding' } } } })
    await flushPromises()
    expect(api.getCNProviderQuota).not.toHaveBeenCalled()
    expect(api.getCNProviderBalance).toHaveBeenCalledTimes(platform === 'minimax' ? 0 : 1)
    expect(wrapper.text()).toEqual(platform === 'minimax' ? '' : expect.stringContaining('12.00'))
    wrapper.unmount()
  })
  it.each([undefined, 'unknown'])('falls back to legacy coding when the local mode is not valid: %s', async mode => {
    api.getCNProviderQuota.mockResolvedValue(quota(35))
    const wrapper = mount(CNProviderUsageCell, { props: { account: { ...account(1, { cn_billing_mode: mode }), credentials: { account_mode: ' coding ' } } } })
    await flushPromises()
    expect(api.getCNProviderQuota).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toContain('35%')
    wrapper.unmount()
  })
  it('switches to balance when local billing mode changes and rejects a pending quota result', async () => {
    const pending = deferred<ReturnType<typeof quota>>()
    api.getCNProviderQuota.mockReturnValue(pending.promise)
    api.getCNProviderBalance.mockResolvedValue({ provider: 'kimi', success: true, balance: 12 })
    const original = { ...account(1), credentials: { account_mode: 'coding' } }
    const wrapper = mount(CNProviderUsageCell, { props: { account: original } })
    const signal = api.getCNProviderQuota.mock.calls[0]?.[1].signal as AbortSignal
    await wrapper.setProps({ account: { ...original, extra: { cn_billing_mode: 'payg' } } })
    pending.resolve(quota(99))
    await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(api.getCNProviderBalance).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toContain('12.00')
    expect(wrapper.text()).not.toContain('99%')
    wrapper.unmount()
  })
  it('does not probe again for unrelated account telemetry changes', async () => {
    api.getCNProviderQuota.mockResolvedValue(quota(35))
    const wrapper = mount(CNProviderUsageCell, { props: { account: account(1) } })
    await flushPromises()
    await wrapper.setProps({ account: { ...account(1), last_used_at: '2026-11-17T12:00:00Z', current_concurrency: 2 } })
    expect(api.getCNProviderQuota).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toContain('35%')
    wrapper.unmount()
  })
  it.each([
    { credentials: { api_protocol: 'adaptive' } },
    { credentials: { api_base_urls: { chat_completions: 'https://example.test/v1' } } },
    { extra: { cn_api_base_urls: { chat_completions: 'https://example.test/v1' } } },
    { credentials: { zhipu_organization: 'org-b' } },
    { credentials: { zhipu_project: 'project-b' } },
    { credentials: { header_override_enabled: true } },
    { credentials: { header_overrides: { 'x-team': 'b' } } },
    { proxy_id: 2 }
  ])('invalidates an old result after a same-account probe configuration change: %j', async change => {
    const first = deferred<ReturnType<typeof quota>>(), second = deferred<ReturnType<typeof quota>>()
    api.getCNProviderQuota.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const original = { ...account(1), platform: 'zhipu' as const }
    const wrapper = mount(CNProviderUsageCell, { props: { account: original } })
    const signal = api.getCNProviderQuota.mock.calls[0]?.[1].signal as AbortSignal
    await wrapper.setProps({ account: { ...original, ...change, extra: { ...original.extra, ...change.extra } } })
    expect(signal.aborted).toBe(true)
    expect(api.getCNProviderQuota).toHaveBeenCalledTimes(2)
    first.resolve(quota(99))
    await flushPromises()
    expect(wrapper.text()).not.toContain('99%')
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    second.resolve(quota(25))
    await flushPromises()
    expect(wrapper.text()).toContain('25%')
    wrapper.unmount()
  })
  it.each([undefined, null, '', NaN, Infinity])('renders unknown percentages without aria-valuenow: %s', value => {
    const wrapper = mount(QuotaUsageBar, { props: { label: '5h', value } })
    expect(wrapper.text()).toContain('—')
    expect(wrapper.get('[role="progressbar"]').attributes('aria-valuenow')).toBeUndefined()
    wrapper.unmount()
  })
})
describe('Ollama request lifecycle', () => {
  it('keeps a newer saved snapshot while the initial GET learns configuration', async () => {
    const pending = deferred<OllamaCloudUsageState>()
    api.getOllamaCloudUsage.mockReturnValue(pending.promise)
    const wrapper = mount(OllamaCloudUsageCell, { props: { account: ollama(1) } })
    const updated = { ...ollama(1), extra: { ollama_cloud_usage_snapshot: state(1, 75).snapshot } }
    await wrapper.setProps({ account: updated })
    pending.resolve(state(1, 10))
    await flushPromises()
    expect(wrapper.text()).toContain('75%')
    expect(wrapper.text()).not.toContain('10%')
    expect(api.getOllamaCloudUsage).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })
  it('retains successful values when a provider failure has no data', async () => {
    api.getOllamaCloudUsage.mockResolvedValue(state(1, 42))
    api.refreshOllamaCloudUsage.mockResolvedValue({ ...state(1, 0), snapshot: { status: 'failed' } })
    const wrapper = mount(OllamaCloudUsageCell, { props: { account: ollama(1) } })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('42%')
    expect(wrapper.text()).toContain('quotaDisplay.failed')
    wrapper.unmount()
  })
  it('rejects another account response and aborts GET on unmount', async () => {
    const pending = deferred<OllamaCloudUsageState>()
    api.getOllamaCloudUsage.mockReturnValue(pending.promise)
    const wrapper = mount(OllamaCloudUsageCell, { props: { account: ollama(1) } })
    const signal = api.getOllamaCloudUsage.mock.calls[0]?.[1].signal as AbortSignal
    pending.resolve(state(2, 99))
    await flushPromises()
    expect(wrapper.text()).not.toContain('99%')
    wrapper.unmount()
    api.getOllamaCloudUsage.mockReturnValue(new Promise(() => {}))
    const next = mount(OllamaCloudUsageCell, { props: { account: ollama(1) } })
    const nextSignal = api.getOllamaCloudUsage.mock.calls[1]?.[1].signal as AbortSignal
    next.unmount()
    expect(nextSignal.aborted).toBe(true)
    expect(signal.aborted).toBe(false)
  })
  it('invalidates an in-flight request when the session revision changes', async () => {
    const old = deferred<OllamaCloudUsageState>()
    api.getOllamaCloudUsage.mockReturnValueOnce(old.promise).mockResolvedValueOnce(state(1, 30))
    const wrapper = mount(OllamaCloudUsageCell, { props: { account: ollama(1) } })
    const signal = api.getOllamaCloudUsage.mock.calls[0]?.[1].signal as AbortSignal
    await wrapper.setProps({ account: { ...ollama(1), extra: { ollama_cloud_usage_session: 'encrypted-revision' } } })
    old.resolve(state(1, 99))
    await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(wrapper.text()).toContain('30%')
    expect(wrapper.text()).not.toContain('99%')
    wrapper.unmount()
  })
  it('only reads on mount and keeps last-good data when refresh fails', async () => {
    api.getOllamaCloudUsage.mockResolvedValue(state(1, 45))
    api.refreshOllamaCloudUsage.mockRejectedValue(new Error('network failure'))
    const wrapper = mount(OllamaCloudUsageCell, { props: { account: ollama(1) } })
    await flushPromises()
    expect(api.refreshOllamaCloudUsage).not.toHaveBeenCalled()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('45%')
    expect(wrapper.text()).toContain('quotaDisplay.failed')
    wrapper.unmount()
  })
  it('does not replay a pending POST or accept its late error after switching accounts', async () => {
    api.getOllamaCloudUsage.mockImplementation((id: number) => Promise.resolve(state(id, id * 10)))
    const pending = deferred<OllamaCloudUsageState>()
    api.refreshOllamaCloudUsage.mockReturnValue(pending.promise)
    const wrapper = mount(OllamaCloudUsageCell, { props: { account: ollama(1) } })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    await wrapper.get('button').trigger('click')
    expect(api.refreshOllamaCloudUsage).toHaveBeenCalledTimes(1)
    await wrapper.setProps({ account: ollama(2) })
    await flushPromises()
    pending.reject(new Error('old account failed'))
    await flushPromises()
    expect(wrapper.text()).toContain('20%')
    expect(wrapper.text()).not.toContain('quotaDisplay.failed')
    wrapper.unmount()
  })
})
