import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { Account } from '@/types'
import AccountTestModal from '../AccountTestModal.vue'

const api = vi.hoisted(() => ({ models: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getAvailableModels: api.models } } }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const account = (id = 42) => ({ id, name: `OpenAI ${id}`, platform: 'openai', type: 'apikey', status: 'active' }) as Account
const models = (...ids: string[]) => ids.map(id => ({ id, display_name: id, type: 'model' }))
function render() {
  return mount(AccountTestModal, {
    props: { show: true, account: account() },
    global: { stubs: {
      BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
      Select: true, TextArea: true, Icon: true
    } }
  })
}

function stalledFetch() {
  const fetch = vi.fn((_url: string, init: RequestInit) => new Promise<Response>((_resolve, reject) => {
    init.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true })
  }))
  vi.stubGlobal('fetch', fetch)
  return fetch
}

beforeEach(() => { api.models.mockResolvedValue(models('gpt-6.2-sol')) })
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); vi.clearAllMocks() })

describe('admin account test lifecycle', () => {
  it('ignores late model results when the account changes', async () => {
    let resolveOld!: (value: ReturnType<typeof models>) => void
    api.models.mockImplementation((id: number) => id === 42 ? new Promise(resolve => { resolveOld = resolve }) : Promise.resolve(models('gpt-6.2-astra')))
    const wrapper = render()
    await wrapper.setProps({ account: account(43) })
    await flushPromises()
    resolveOld(models('gpt-6.2-sol'))
    await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.availableModels.map((model: { id: string }) => model.id)).toEqual(['gpt-6.2-astra'])
    expect(vm.selectedModelId).toBe('gpt-6.2-astra')
    wrapper.unmount()
  })

  it('bounds a stalled OpenAI text request and clears its timer', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    stalledFetch()
    const wrapper = render()
    try {
      await flushPromises()
      const vm = wrapper.vm as any
      const pending = vm.startTest()
      await vi.advanceTimersByTimeAsync(190_000)
      await pending
      expect(vm.status).toBe('error')
      expect(wrapper.text()).toContain('admin.accounts.testTimedOut')
      expect(vi.getTimerCount()).toBe(0)
    } finally { wrapper.unmount() }
  })

  it.each(['gpt-image-2', 'gemini-3-pro-image-preview'])('keeps the text timeout off for an alias mapped to %s', async upstreamModel => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    api.models.mockResolvedValue([{ id: 'image-alias', upstream_model: upstreamModel, display_name: 'Image' }])
    const fetch = stalledFetch()
    const wrapper = render()
    try {
      await flushPromises()
      const vm = wrapper.vm as any
      vm.testPrompt = 'draw a cat'
      const pending = vm.startTest()
      await vi.advanceTimersByTimeAsync(190_000)
      expect(fetch.mock.calls[0][1].signal?.aborted).toBe(false)
      expect(JSON.parse(fetch.mock.calls[0][1].body as string)).toMatchObject({ model_id: 'image-alias', prompt: 'draw a cat' })
      expect(vm.status).toBe('connecting')
      vm.cancelTest()
      await pending
      expect(vm.status).toBe('idle')
    } finally { wrapper.unmount() }
  })

  it('aborts the test on unmount', async () => {
    const fetch = stalledFetch()
    const wrapper = render()
    await flushPromises()
    const pending = (wrapper.vm as any).startTest()
    wrapper.unmount()
    await pending
    expect(fetch.mock.calls[0][1].signal?.aborted).toBe(true)
    expect(wrapper.emitted('account-updated')).toBeUndefined()
  })

  it('reports EOF without a terminal result', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('data:{"type":"content","text":"partial"}\n\n')))
    const wrapper = render()
    await flushPromises()
    await (wrapper.vm as any).startTest()
    expect(wrapper.text()).toContain('admin.accounts.testStreamIncomplete')
    expect(wrapper.text()).not.toContain('admin.accounts.testCompleted')
    wrapper.unmount()
  })

  it.each([false, true])('cancels an oversized OpenAI stream (complete lines: %s)', async completeLines => {
    const chunk = new Uint8Array(8 * 1024 * 1024).fill(32)
    if (completeLines) chunk[chunk.length - 1] = 10
    const cancel = vi.fn()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(new ReadableStream({
      pull(controller) { controller.enqueue(chunk) },
      cancel
    }))))
    const wrapper = render()
    try {
      await flushPromises()
      await (wrapper.vm as any).startTest()
      expect(wrapper.text()).toContain('admin.accounts.testStreamTooLarge')
      expect(cancel).toHaveBeenCalledOnce()
      expect((wrapper.vm as any).status).toBe('error')
    } finally { wrapper.unmount() }
  })

  it('bounds output rows while retaining the result and trailing account refresh', async () => {
    const events = Array.from({ length: 2100 }, (_, i) => `data:{"type":"status","text":"row-${i}"}\n`)
    events.push('data:{"type":"test_complete","success":true}\n')
    events.push('data:{"type":"account_updated","data":{"id":42,"status":"active"}}\n')
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(events.join(''))))
    const wrapper = render()
    try {
      await flushPromises()
      await (wrapper.vm as any).startTest()
      const vm = wrapper.vm as any
      expect(vm.outputLines).toHaveLength(2000)
      expect(vm.outputLines[0].text).toBe('row-100')
      expect(vm.outputLines[1999].text).toBe('row-2099')
      expect(vm.status).toBe('success')
      expect(wrapper.emitted('account-updated')).toEqual([[{ id: 42, status: 'active' }]])
    } finally { wrapper.unmount() }
  })

  it('ignores a late stream result after switching accounts', async () => {
    let stream!: ReadableStreamDefaultController<Uint8Array>
    const cancel = vi.fn()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(new ReadableStream({
      start(controller) { stream = controller },
      cancel
    }))))
    const wrapper = render()
    try {
      await flushPromises()
      const pending = (wrapper.vm as any).startTest()
      await flushPromises()
      await wrapper.setProps({ account: account(43) })
      stream.enqueue(new TextEncoder().encode('data:{"type":"test_complete","success":true}\ndata:{"type":"account_updated","data":{"id":42}}\n'))
      await pending
      expect((wrapper.vm as any).status).toBe('idle')
      expect(wrapper.emitted('account-updated')).toBeUndefined()
      expect(cancel).toHaveBeenCalledOnce()
    } finally { wrapper.unmount() }
  })
})
