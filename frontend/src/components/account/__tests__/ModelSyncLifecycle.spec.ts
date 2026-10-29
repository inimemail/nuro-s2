import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import GrokModelDiagnostics from '../GrokModelDiagnostics.vue'
import ModelWhitelistSelector from '../ModelWhitelistSelector.vue'

const { diagnostics, sync, notify, copy } = vi.hoisted(() => ({
  diagnostics: vi.fn(), sync: vi.fn(), notify: vi.fn(), copy: vi.fn()
}))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/accounts', () => ({
  getGrokModelDiagnostics: diagnostics,
  accountsAPI: { syncUpstreamModels: sync }
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: notify, showSuccess: notify, showInfo: notify })
}))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: copy }) }))

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(done => { resolve = done })
  return { promise, resolve }
}
function button(wrapper: ReturnType<typeof mount>, text: string) {
  const found = wrapper.findAll('button').find(node => node.text() === text)
  expect(found, text).toBeDefined()
  return found!
}
const snapshot = (count: number) => ({ status: 'success', catalog_count: count, models: [] })
beforeEach(() => { vi.clearAllMocks() })

describe('saved Grok diagnostics', () => {
  it('copies individual model IDs and cancels pending inspection without stale results', async () => {
    diagnostics.mockResolvedValueOnce({ ...snapshot(1), models: [{ model: 'grok-model-alias', status: 'unverifiable' }] })
    const wrapper = mount(GrokModelDiagnostics, { props: { accountId: 1, active: true } })
    await button(wrapper, 'admin.accounts.grokCatalog.inspect').trigger('click')
    await flushPromises()
    await button(wrapper, 'common.copy').trigger('click')
    expect(copy).toHaveBeenCalledWith('grok-model-alias')
    const pending = deferred<ReturnType<typeof snapshot>>()
    diagnostics.mockReturnValueOnce(pending.promise)
    await button(wrapper, 'admin.accounts.grokCatalog.inspect').trigger('click')
    const signal = diagnostics.mock.calls[1][1] as AbortSignal
    await button(wrapper, 'common.cancel').trigger('click')
    expect(signal.aborted).toBe(true)
    pending.resolve(snapshot(99))
    await flushPromises()
    expect(wrapper.find('[aria-live]').exists()).toBe(false)
    expect(button(wrapper, 'admin.accounts.grokCatalog.inspect').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
  it('does no request until explicitly inspected and aborts when the account changes', async () => {
    const first = deferred<ReturnType<typeof snapshot>>()
    const second = deferred<ReturnType<typeof snapshot>>()
    diagnostics.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const wrapper = mount(GrokModelDiagnostics, { props: { accountId: 1, active: true } })
    expect(diagnostics).not.toHaveBeenCalled()
    await wrapper.get('button').trigger('click')
    const signal = diagnostics.mock.calls[0][1] as AbortSignal
    await wrapper.setProps({ accountId: 2 })
    expect(signal.aborted).toBe(true)
    await wrapper.get('button').trigger('click')
    first.resolve(snapshot(10))
    await flushPromises()
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    expect(wrapper.find('[aria-live]').exists()).toBe(false)
    second.resolve(snapshot(20))
    await flushPromises()
    expect(wrapper.find('[aria-live]').exists()).toBe(true)
    wrapper.unmount()
  })
  it('does not render a response after closing or unmounting', async () => {
    const response = deferred<ReturnType<typeof snapshot>>()
    diagnostics.mockReturnValue(response.promise)
    const wrapper = mount(GrokModelDiagnostics, { props: { accountId: 1, active: true } })
    await wrapper.get('button').trigger('click')
    await wrapper.setProps({ active: false })
    response.resolve(snapshot(1))
    await flushPromises()
    expect(wrapper.find('[aria-live]').exists()).toBe(false)
    wrapper.unmount()
    expect((diagnostics.mock.calls[0][1] as AbortSignal).aborted).toBe(true)
  })
})

describe('model whitelist sync', () => {
  const create = () => mount(ModelWhitelistSelector, {
    props: { accountId: 1, platform: 'openai', modelValue: ['original'], syncActive: true },
    global: { stubs: { ModelIcon: true, Icon: true } }
  })
  it('preserves edits made during sync until the user explicitly merges', async () => {
    const response = deferred<{ models: string[] }>()
    sync.mockReturnValue(response.promise)
    const wrapper = create()
    await button(wrapper, 'admin.accounts.syncUpstreamModels').trigger('click')
    await wrapper.setProps({ modelValue: ['draft'] })
    response.resolve({ models: ['returned', 'returned', ' draft '] })
    await flushPromises()
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.find('[role="status"]').exists()).toBe(true)
    await button(wrapper, 'admin.accounts.applyUpstreamModels').trigger('click')
    expect(wrapper.emitted('update:modelValue')).toEqual([[['draft', 'returned']]])
    wrapper.unmount()
  })
  it('ignores a cancelled response without clearing a newer loading state', async () => {
    const first = deferred<{ models: string[] }>()
    const second = deferred<{ models: string[] }>()
    sync.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const wrapper = create()
    await button(wrapper, 'admin.accounts.syncUpstreamModels').trigger('click')
    const signal = sync.mock.calls[0][1].signal as AbortSignal
    await button(wrapper, 'common.cancel').trigger('click')
    expect(signal.aborted).toBe(true)
    await button(wrapper, 'admin.accounts.syncUpstreamModels').trigger('click')
    first.resolve({ models: ['stale'] })
    await flushPromises()
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(button(wrapper, 'admin.accounts.syncUpstreamModelsLoading').attributes('disabled')).toBeDefined()
    second.resolve({ models: ['current'] })
    await flushPromises()
    expect(wrapper.emitted('update:modelValue')).toEqual([[['original', 'current']]])
    wrapper.unmount()
  })
  it('aborts and discards pending results on close', async () => {
    const response = deferred<{ models: string[] }>()
    sync.mockReturnValue(response.promise)
    const wrapper = create()
    await button(wrapper, 'admin.accounts.syncUpstreamModels').trigger('click')
    await wrapper.setProps({ syncActive: false })
    response.resolve({ models: ['late'] })
    await flushPromises()
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
    wrapper.unmount()
  })
})
