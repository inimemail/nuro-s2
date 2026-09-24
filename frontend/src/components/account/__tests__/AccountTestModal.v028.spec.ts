import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, ref } from 'vue'
import type { Account } from '@/types'
import AccountTestModal from '../AccountTestModal.vue'

const api = vi.hoisted(() => ({ models: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getAvailableModels: api.models } } }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, locale: ref('en') }) }))
const account = (id: number, platform = 'openai') => ({ id, name: `Account ${id}`, platform, type: 'apikey', credentials: {}, extra: {} }) as Account
const models = (...ids: string[]) => ids.map(id => ({ id, display_name: id, type: 'model' }))
const SelectStub = defineComponent({ props: ['options', 'modelValue', 'valueKey', 'labelKey'], template: `<select><option v-for="option in options" :key="option[valueKey || 'value']" :value="option[valueKey || 'value']">{{ option[labelKey || 'label'] }}</option></select>` })
function render(id = 1, platform = 'openai') {
  return mount(AccountTestModal, { props: { show: true, account: account(id, platform) }, global: { stubs: { BaseDialog: { template: '<div><slot/><slot name="footer"/></div>' }, Select: SelectStub, Icon: true, TextArea: true } } })
}
afterEach(() => { vi.unstubAllGlobals(); vi.clearAllMocks() })
describe('v0.2.8 account test modal', () => {
  it('places supported new models first without inserting unavailable models', async () => {
    api.models.mockResolvedValue(models('gpt-5.6', 'gpt-6-astra', 'gpt-6-luna', 'gpt-6-sol'))
    const wrapper = render(); await flushPromises()
    const options = wrapper.findAll('option').map(option => option.text()).filter(text => text.startsWith('gpt-'))
    expect(options).toEqual(['gpt-6-sol', 'gpt-6-luna', 'gpt-6-astra', 'gpt-5.6'])
    wrapper.unmount()
  })
  it('ignores old model results after switching accounts', async () => {
    let resolveOld!: (value: ReturnType<typeof models>) => void
    api.models.mockImplementation((id: number) => id === 1 ? new Promise(resolve => { resolveOld = resolve }) : Promise.resolve(models('gpt-6-luna')))
    const wrapper = render(); await wrapper.setProps({ account: account(2) }); await flushPromises()
    resolveOld(models('gpt-6-sol')); await flushPromises()
    expect(wrapper.text()).toContain('gpt-6-luna'); expect(wrapper.text()).not.toContain('gpt-6-sol')
    wrapper.unmount()
  })
  it('stops on a complete test event without waiting for EOF and cancels the reader', async () => {
    api.models.mockResolvedValue(models('gpt-6-sol'))
    const cancel = vi.fn()
    const stream = new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode('data:{"type":"test_complete","success":true}\n\n')) }, cancel })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(stream)))
    const wrapper = render(); await flushPromises()
    const start = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.startTest'))!
    await start.trigger('click'); await flushPromises()
    expect(cancel).toHaveBeenCalledOnce()
    expect(wrapper.text()).not.toContain('admin.accounts.testing')
    wrapper.unmount()
  })
  it('reports EOF without a terminal event', async () => {
    api.models.mockResolvedValue(models('gpt-6-sol'))
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('data: {"type":"content","text":"partial"}\n\n')))
    const wrapper = render(); await flushPromises()
    await wrapper.findAll('button').find(button => button.text().includes('admin.accounts.startTest'))!.trigger('click')
    await flushPromises(); expect(wrapper.text()).toContain('admin.accounts.testStreamIncomplete')
    wrapper.unmount()
  })
  it('ignores trailing data after the first terminal event', async () => {
    api.models.mockResolvedValue(models('gpt-6-sol'))
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('data:{"type":"test_complete","success":true}\n\ndata:{"type":"error","error":"late failure"}')))
    const wrapper = render(); await flushPromises()
    await wrapper.findAll('button').find(button => button.text().includes('admin.accounts.startTest'))!.trigger('click')
    await flushPromises()
    expect(wrapper.text()).not.toContain('late failure')
    expect(wrapper.text()).toContain('admin.accounts.testCompleted')
    wrapper.unmount()
  })
})
