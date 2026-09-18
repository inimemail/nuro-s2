import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AdminModal from '../AccountTestModal.vue'
import AccountModal from '@/components/account/AccountTestModal.vue'

const { getAvailableModels } = vi.hoisted(() => ({ getAvailableModels: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getAvailableModels } } }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const models = [
  { id: 'custom-picture', upstream_model: 'grok-imagine-image-2.0', display_name: 'Picture' },
  { id: 'grok-imagine-video', display_name: 'Video' },
  { id: 'grok-4.6', display_name: 'Grok 4.6' }
]

describe.each([['admin', AdminModal], ['account', AccountModal]] as const)('%s Grok test modal', (_, component) => {
  beforeEach(() => {
    getAvailableModels.mockResolvedValue(models)
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true,
      body: { getReader: () => {
        let done = false
        return { read: async () => {
          if (done) return { done: true }
          done = true
          return { done: false, value: new TextEncoder().encode('data: {"type":"test_complete","success":true}\n\n') }
        } }
      } }
    }))
    localStorage.setItem('auth_token', 'test-token')
  })
  afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks() })

  async function openModal() {
    const wrapper = mount(component, {
      props: { show: false, account: { id: 42, name: 'Grok', platform: 'grok', type: 'apikey', status: 'active' } as never },
      global: { stubs: {
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
        Select: {
          props: ['options', 'modelValue', 'valueKey', 'labelKey'],
          emits: ['update:modelValue'],
          template: '<select :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option v-for="option in options" :key="option[valueKey || \'value\']" :value="option[valueKey || \'value\']">{{ option[labelKey || \'label\'] }}</option></select>'
        },
        TextArea: { props: ['modelValue'], emits: ['update:modelValue'], template: '<textarea :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />' },
        Icon: true
      } }
    })
    await wrapper.setProps({ show: true })
    await flushPromises()
    return wrapper
  }

  it('filters text/media models, classifies mapped aliases, and switches model with mode', async () => {
    const wrapper = await openModal()
    expect(wrapper.findAll('select')[0].findAll('option').map(o => o.attributes('value'))).toEqual(['grok-4.6'])
    await wrapper.findAll('select')[1].setValue('image')
    expect(wrapper.findAll('select')[0].findAll('option').map(o => o.attributes('value'))).toEqual(['custom-picture'])
    await wrapper.find('textarea').setValue('draw a cat')
    await wrapper.findAll('button').find(b => b.text().includes('admin.accounts.startTest'))!.trigger('click')
    expect(JSON.parse(vi.mocked(fetch).mock.calls[0][1]!.body as string)).toMatchObject({ model_id: 'custom-picture', mode: 'image', prompt: 'draw a cat' })
    wrapper.unmount()
  })

  it('video uploads an image without replacing the prompt; changing mode clears media', async () => {
    const wrapper = await openModal()
    await wrapper.findAll('select')[1].setValue('video')
    await wrapper.find('textarea').setValue('a cat walking')
    const input = wrapper.find('input[type="file"]')
    expect(input.attributes('accept')).toBe('image/*')
    Object.defineProperty(input.element, 'files', { value: [new File(['image'], 'frame.png', { type: 'image/png' })] })
    await input.trigger('change')
    await vi.waitFor(() => expect(wrapper.text()).toContain('frame.png'))
    expect((wrapper.find('textarea').element as HTMLTextAreaElement).value).toBe('a cat walking')
    await wrapper.findAll('button').find(b => b.text().includes('admin.accounts.startTest'))!.trigger('click')
    const request = JSON.parse(vi.mocked(fetch).mock.calls[0][1]!.body as string)
    expect(request).toMatchObject({ model_id: 'grok-imagine-video', prompt: 'a cat walking', mode: 'video' })
    expect(request.image_data_url).toMatch(/^data:image\/png;base64,/)
    await flushPromises()
    await wrapper.findAll('select')[1].setValue('text')
    expect(wrapper.text()).not.toContain('frame.png')
    wrapper.unmount()
  })

  it('voice tests do not submit a text model and STT requires an audio file', async () => {
    const wrapper = await openModal()
    await wrapper.findAll('select')[1].setValue('stt')
    expect(wrapper.findAll('select')).toHaveLength(1)
    expect(wrapper.findAll('button').find(b => b.text().includes('admin.accounts.startTest'))!.attributes('disabled')).toBeDefined()
    await wrapper.find('select').setValue('tts')
    await wrapper.findAll('button').find(b => b.text().includes('admin.accounts.startTest'))!.trigger('click')
    expect(JSON.parse(vi.mocked(fetch).mock.calls[0][1]!.body as string)).toMatchObject({ model_id: '', mode: 'tts' })
    wrapper.unmount()
  })
})
