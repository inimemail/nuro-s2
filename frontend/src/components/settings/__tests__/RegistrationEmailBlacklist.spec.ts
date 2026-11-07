import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'
import RegistrationEmailBlacklist from '../RegistrationEmailBlacklist.vue'

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get, post } }))
const page = { revision: 'saved-revision', users: [{ id: 1, email: 'fixture@example.com', updated_at: '2026-11-07T00:00:00Z' }], has_more: false, next_id: 1 }
beforeEach(() => { get.mockReset(); post.mockReset() })
afterEach(() => { vi.unstubAllGlobals() })
function render(locale: 'en' | 'zh' = 'en') {
  const i18n = createI18n({ legacy: false, locale, messages: { en, zh } })
  return mount(RegistrationEmailBlacklist, {
    props: { modelValue: ['fixture@example.com'], saved: ['fixture@example.com'] },
    global: { plugins: [i18n] }
  })
}

describe('registration blacklist confirmation', () => {
  it('submits a valid, stable batch UUID over HTTP when randomUUID is unavailable', async () => {
    const getRandomValues = globalThis.crypto.getRandomValues.bind(globalThis.crypto)
    vi.stubGlobal('crypto', { getRandomValues })
    get.mockResolvedValue({ data: page })
    post.mockRejectedValueOnce(new Error('Connection lost')).mockResolvedValueOnce({ data: { disabled_ids: [1] } })
    const wrapper = render()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).not.toContain(en.upgradeBlacklist.failed)
    await wrapper.get('input[type=checkbox]').setValue(true)
    await wrapper.findAll('button').at(-1)!.trigger('click')
    await flushPromises()
    await wrapper.findAll('button').at(-1)!.trigger('click')
    await flushPromises()
    expect(post).toHaveBeenCalledTimes(2)
    const batchID = post.mock.calls[0][1].batch_id
    expect(batchID).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
    expect(post.mock.calls[1][1].batch_id).toBe(batchID)
    wrapper.unmount()
  })
  it.each(['en', 'zh'] as const)('renders literal email examples using the real %s translation compiler', locale => {
    const errors = vi.spyOn(console, 'error')
    const wrapper = render(locale)
    expect(wrapper.get('textarea').attributes('placeholder')).toBe('name@example.com\nother@gmail.com')
    expect(errors).not.toHaveBeenCalled()
    wrapper.unmount()
    errors.mockRestore()
  })
  it('requires saved rules and explicit confirmation before submitting the preview', async () => {
    get.mockResolvedValue({ data: page })
    post.mockResolvedValue({ data: { disabled_ids: [1] } })
    const wrapper = render()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('button').at(-1)?.attributes('disabled')).toBeDefined()
    expect(post).not.toHaveBeenCalled()
    await wrapper.get('input[type=checkbox]').setValue(true)
    await wrapper.findAll('button').at(-1)!.trigger('click')
    await flushPromises()
    expect(post).toHaveBeenCalledTimes(1)
    expect(post.mock.calls[0][1]).toMatchObject({ revision: page.revision, users: page.users, confirm: true })
    expect(post.mock.calls[0][1].batch_id).toMatch(/^[0-9a-f-]{36}$/)
    expect(wrapper.find('input[type=checkbox]').exists()).toBe(false)
    wrapper.unmount()
  })
  it('aborts and ignores an obsolete preview when rules change', async () => {
    let resolve!: (value: unknown) => void
    get.mockReturnValue(new Promise(done => { resolve = done }))
    const wrapper = render()
    await wrapper.get('button').trigger('click')
    const signal = get.mock.calls[0][1].signal as AbortSignal
    await wrapper.setProps({ modelValue: ['different@example.com'] })
    expect(signal.aborted).toBe(true)
    resolve({ data: page })
    await flushPromises()
    expect(wrapper.find('input[type=checkbox]').exists()).toBe(false)
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    expect(post).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
