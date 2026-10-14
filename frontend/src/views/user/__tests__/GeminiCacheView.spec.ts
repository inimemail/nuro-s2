import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import GeminiCacheView from '../GeminiCacheView.vue'
import { readPendingCacheOperation, savePendingCacheOperation } from '@/utils/geminiCachePending'
const api = vi.hoisted(() => ({ listCacheKeys: vi.fn(), listCaches: vi.fn(), quoteCache: vi.fn(), createCache: vi.fn(), quoteRenewCache: vi.fn(), renewCache: vi.fn(), deleteCache: vi.fn(), cacheOperationStatus: vi.fn() }))
vi.mock('@/api/geminiCache', async () => ({ ...await vi.importActual('@/api/geminiCache'), ...api }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: { id: 3 } }) }))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual('vue-i18n'), useI18n: () => ({ t: (key: string) => key, locale: { value: 'zh' } }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))
const mountPage = () => mount(GeminiCacheView, { global: { stubs: {
  AppLayout: { template: '<main><slot /></main>' }, Icon: true,
  BaseDialog: { props: ['show'], template: '<section v-if="show"><slot /><slot name="footer" /></section>' },
  Select: { props: ['modelValue', 'options'], template: '<select :value="modelValue" @change="$emit(\'update:modelValue\', Number($event.target.value))"><option v-for="o in options" :key="o.value" :value="o.value">{{o.label}}</option></select>' }
} } })
const button = (wrapper: ReturnType<typeof mountPage>, text: string) => wrapper.findAll('button').find(b => b.text().includes(text))!
describe('Gemini cache management safeguards', () => {
  beforeEach(() => {
    localStorage.clear(); vi.resetAllMocks()
    api.listCacheKeys.mockResolvedValue({ items: [{ id: 7, name: 'Gemini', group: { name: 'Test' } }], page: 1, pages: 2 })
    api.listCaches.mockResolvedValue({ cachedContents: [] })
    api.quoteCache.mockResolvedValue({ amount: '0.1', tokens: 1000, inputCost: '0.01', storageCost: '0.09' })
  })
  it('retains one idempotency ID on an unknown create and protects the form', async () => {
    api.createCache.mockRejectedValueOnce({ status: 503 }).mockResolvedValueOnce({})
    const wrapper = mountPage(); await flushPromises()
    await button(wrapper, 'geminiCache.create').trigger('click')
    await wrapper.get('#cache-content').setValue('context')
    await button(wrapper, 'geminiCache.preview').trigger('click'); await flushPromises()
    await wrapper.get('#gemini-cache-form').trigger('submit'); await flushPromises()
    const first = api.createCache.mock.calls[0]!
    expect(readPendingCacheOperation(3)?.id).toBe(first[2])
    expect(wrapper.get('#cache-content').attributes('disabled')).toBeDefined()
    expect(button(wrapper, 'common.cancel').attributes('disabled')).toBeDefined()
    await wrapper.get('#gemini-cache-form').trigger('submit'); await flushPromises()
    expect(api.createCache.mock.calls[1]![2]).toBe(first[2])
    expect(api.createCache.mock.calls[1]![1]).toEqual(first[1])
    expect(readPendingCacheOperation(3)).toBeNull()
    wrapper.unmount()
  })
  it('restores unknown operations without storing or resending their body', async () => {
    savePendingCacheOperation(3, { id: 'a'.repeat(32), keyID: 7, kind: 'create', createdAt: Date.now() })
    api.cacheOperationStatus.mockResolvedValue({ state: 'send_started' })
    const wrapper = mountPage(); await flushPromises()
    expect(api.cacheOperationStatus).toHaveBeenCalledWith(7, 'a'.repeat(32), 'create')
    expect(api.createCache).not.toHaveBeenCalled()
    expect(button(wrapper, 'geminiCache.create').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain('geminiCache.checkPending')
    wrapper.unmount()
  })
  it('keeps an unknown operation after a retry is rejected by a quota check', async () => {
    api.createCache.mockRejectedValueOnce({ status: 503 }).mockRejectedValueOnce({ status: 429 })
    const wrapper = mountPage(); await flushPromises()
    await button(wrapper, 'geminiCache.create').trigger('click')
    await wrapper.get('#cache-content').setValue('context')
    await button(wrapper, 'geminiCache.preview').trigger('click'); await flushPromises()
    await wrapper.get('#gemini-cache-form').trigger('submit'); await flushPromises()
    const pending = readPendingCacheOperation(3)
    await wrapper.get('#gemini-cache-form').trigger('submit'); await flushPromises()
    expect(api.createCache.mock.calls[1]![2]).toBe(pending?.id)
    expect(readPendingCacheOperation(3)).toEqual(pending)
    expect(button(wrapper, 'common.cancel').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })
  it('shows key pagination even when the current page has no Gemini keys', async () => {
    api.listCacheKeys.mockResolvedValueOnce({ items: [], page: 1, pages: 2 })
    const wrapper = mountPage(); await flushPromises()
    await button(wrapper, 'geminiCache.nextKeys').trigger('click'); await flushPromises()
    expect(api.listCacheKeys.mock.calls[1]![0]).toBe(2)
    wrapper.unmount()
  })
  it('does not clear a newer operation when an old status query finishes', async () => {
    const first = { id: 'a'.repeat(32), keyID: 7, kind: 'create' as const, createdAt: Date.now() }
    const next = { ...first, id: 'b'.repeat(32), kind: 'delete' as const }
    savePendingCacheOperation(3, first)
    let finish!: (value: { state: string }) => void
    api.cacheOperationStatus.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const wrapper = mountPage(); await flushPromises()
    savePendingCacheOperation(3, next)
    window.dispatchEvent(new StorageEvent('storage'))
    finish({ state: 'settled' }); await flushPromises()
    expect(readPendingCacheOperation(3)).toEqual(next)
    expect(button(wrapper, 'geminiCache.create').attributes('disabled')).toBeDefined()
    api.cacheOperationStatus.mockResolvedValue({ state: 'send_started' })
    await button(wrapper, 'geminiCache.checkPending').trigger('click'); await flushPromises()
    expect(api.cacheOperationStatus).toHaveBeenLastCalledWith(7, next.id, 'delete')
    wrapper.unmount()
  })
})
