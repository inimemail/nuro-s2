import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import AccountTestModal from '../AccountTestModal.vue'

const { getAvailableModelsMock } = vi.hoisted(() => ({
  getAvailableModelsMock: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getAvailableModels: getAvailableModelsMock
    }
  }
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard: vi.fn()
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: { type: [String, Number, Boolean, null], default: '' },
    options: { type: Array, default: () => [] },
    valueKey: { type: String, default: 'value' },
    labelKey: { type: String, default: 'label' }
  },
  emits: ['update:modelValue'],
  template: `
    <select
      v-bind="$attrs"
      :value="modelValue"
      @change="$emit('update:modelValue', $event.target.value)"
    >
      <option
        v-for="option in options"
        :key="option[valueKey]"
        :value="option[valueKey]"
      >
        {{ option[labelKey] }}
      </option>
    </select>
  `
})

const TextAreaStub = defineComponent({
  name: 'TextArea',
  props: {
    modelValue: { type: String, default: '' }
  },
  emits: ['update:modelValue'],
  template: `
    <textarea
      v-bind="$attrs"
      :value="modelValue"
      @input="$emit('update:modelValue', $event.target.value)"
    />
  `
})

function buildAccount() {
  return {
    id: 1,
    name: 'OpenAI OAuth',
    platform: 'openai',
    type: 'oauth',
    status: 'active',
    credentials: {},
    extra: {},
    concurrency: 1,
    priority: 1,
    proxy_id: null,
    auto_pause_on_expired: false
  } as any
}

describe('AccountTestModal', () => {
  const originalFetch = global.fetch

  beforeEach(() => {
    getAvailableModelsMock.mockReset()
    getAvailableModelsMock.mockResolvedValue([
      { id: 'gpt-5.4', display_name: 'GPT-5.4' }
    ])
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: vi.fn().mockResolvedValue({ done: true, value: undefined })
        })
      }
    } as any)
    localStorage.setItem('auth_token', 'test-token')
  })

  afterEach(() => {
    global.fetch = originalFetch
    localStorage.clear()
  })

  it.each([
    { credentials: { seedance_enabled: true }, visible: true },
    { credentials: { openai_capabilities: [' Seedance '] }, visible: true },
    { credentials: { openai_capabilities: { seedance: true } }, visible: true },
    { credentials: { seedance_enabled: false, openai_capabilities: ['seedance'] }, visible: false },
    { credentials: {}, visible: false }
  ])('uses the backend Seedance capability precedence in the test panel: %j', async ({ credentials, visible }) => {
    const account = buildAccount()
    account.type = 'apikey'
    account.credentials = credentials
    const wrapper = mount(AccountTestModal, {
      props: { show: true, account },
      global: { stubs: { BaseDialog: BaseDialogStub, Select: SelectStub, TextArea: TextAreaStub, Icon: true, SeedanceTaskTester: true } }
    })
    await flushPromises()
    expect(wrapper.findComponent({ name: 'SeedanceTaskTester' }).exists()).toBe(visible)
    expect(global.fetch).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it.each(['compact', 'compact_legacy'])('posts %s mode for the selected Compact protocol', async (mode) => {
    const wrapper = mount(AccountTestModal, {
      props: {
        show: true,
        account: buildAccount()
      },
      global: {
        stubs: {
          BaseDialog: BaseDialogStub,
          Select: SelectStub,
          TextArea: TextAreaStub,
          Icon: true
        }
      }
    })

    await flushPromises()
    ;(wrapper.vm as any).selectedModelId = 'gpt-5.4'
    ;(wrapper.vm as any).testMode = mode
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(global.fetch).toHaveBeenCalledTimes(1)
    const [, options] = (global.fetch as any).mock.calls[0]
    expect(JSON.parse(options.body)).toMatchObject({
      model_id: 'gpt-5.4',
      mode
    })
  })

  it('puts GPT-6 aliases first and selects the first model by default', async () => {
    getAvailableModelsMock.mockResolvedValue([
      { id: 'gpt-5.6', display_name: 'GPT-5.6 (Sol)' },
      { id: 'gpt-6', display_name: 'GPT-6 (Astra)' },
      { id: 'gpt-6-astra', display_name: 'GPT-6 Astra' },
      { id: 'gpt-5.4', display_name: 'GPT-5.4' }
    ])

    const wrapper = mount(AccountTestModal, {
      props: {
        show: false,
        account: buildAccount()
      },
      global: {
        stubs: {
          BaseDialog: BaseDialogStub,
          Select: SelectStub,
          TextArea: TextAreaStub,
          Icon: true
        }
      }
    })

    await wrapper.setProps({ show: true })
    await flushPromises()

    const vm = wrapper.vm as any
    expect(vm.availableModels.map((model: { id: string }) => model.id)).toEqual([
      'gpt-6-astra',
      'gpt-6',
      'gpt-5.6',
      'gpt-5.4'
    ])
    expect(vm.selectedModelId).toBe('gpt-6-astra')
  })

  it('renders Chat Completions path status from test SSE', async () => {
    const encoder = new TextEncoder()
    const chunks = [
      encoder.encode('data: {"type":"status","text":"已通过 /v1/chat/completions 验证"}\n\n'),
      encoder.encode('data: {"type":"test_complete","success":true}\n\n')
    ]
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: vi.fn().mockImplementation(() => Promise.resolve(
            chunks.length > 0
              ? { done: false, value: chunks.shift() }
              : { done: true, value: undefined }
          ))
        })
      }
    } as any)

    const wrapper = mount(AccountTestModal, {
      props: {
        show: true,
        account: buildAccount()
      },
      global: {
        stubs: {
          BaseDialog: BaseDialogStub,
          Select: SelectStub,
          TextArea: TextAreaStub,
          Icon: true
        }
      }
    })

    await flushPromises()
    ;(wrapper.vm as any).selectedModelId = 'gpt-5.4'
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(wrapper.text()).toContain('已通过 /v1/chat/completions 验证')
  })
  it('allows cancelling an in-flight test without closing the modal', async () => {
    const wrapper = mount(AccountTestModal, {
      props: { show: true, account: buildAccount() },
      global: { stubs: { BaseDialog: BaseDialogStub, Select: SelectStub, TextArea: TextAreaStub, Icon: true } }
    })
    await flushPromises()
    global.fetch = vi.fn((_url, options) => new Promise((_resolve, reject) => {
      options?.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')))
    })) as any
    const pending = (wrapper.vm as any).startTest()
    await flushPromises()
    expect((wrapper.vm as any).status).toBe('connecting')
    await wrapper.findAll('button').find(button => button.text() === 'common.cancel')!.trigger('click')
    await pending
    expect((wrapper.vm as any).status).toBe('idle')
    expect(wrapper.text()).toContain('admin.accounts.testCancelled')
    expect(wrapper.emitted('close')).toBeUndefined()
    wrapper.unmount()
  })

  it('bounds a stalled OpenAI text test and shows a timeout error', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const wrapper = mount(AccountTestModal, {
      props: { show: true, account: buildAccount() },
      global: { stubs: { BaseDialog: BaseDialogStub, Select: SelectStub, TextArea: TextAreaStub, Icon: true } }
    })
    try {
      await flushPromises()
      global.fetch = vi.fn((_url, options) => new Promise((_resolve, reject) => {
        options?.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')))
      })) as any
      const pending = (wrapper.vm as any).startTest()
      await vi.advanceTimersByTimeAsync(190_000)
      await pending
      expect((wrapper.vm as any).status).toBe('error')
      expect(wrapper.text()).toContain('admin.accounts.testTimedOut')
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })

  it.each(['gpt-image-2', 'gemini-3-pro-image-preview'])('keeps the text timeout off for an image alias mapped to %s', async (upstreamModel) => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    getAvailableModelsMock.mockResolvedValue([{ id: 'my-image-alias', upstream_model: upstreamModel }])
    const account = buildAccount()
    account.type = 'apikey'
    const wrapper = mount(AccountTestModal, {
      props: { show: true, account },
      global: { stubs: { BaseDialog: BaseDialogStub, Select: SelectStub, TextArea: TextAreaStub, Icon: true } }
    })
    try {
      await flushPromises()
      const vm = wrapper.vm as any
      expect(vm.supportsImageTest).toBe(true)
      vm.testPrompt = 'draw a cat'
      let signal: AbortSignal | undefined
      global.fetch = vi.fn((_url, options) => new Promise((_resolve, reject) => {
        signal = options?.signal as AbortSignal
        signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')))
      })) as any
      const pending = vm.startTest()
      await vi.advanceTimersByTimeAsync(190_000)
      expect(signal?.aborted).toBe(false)
      expect(vm.status).toBe('connecting')
      const [, options] = (global.fetch as any).mock.calls[0]
      expect(JSON.parse(options.body)).toMatchObject({ model_id: 'my-image-alias', prompt: 'draw a cat' })
      vm.cancelTest()
      await pending
      expect(vm.status).toBe('idle')
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })

})
