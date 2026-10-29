import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import AdminAccessKeysSettings from '../AdminAccessKeysSettings.vue'
import type { AccessKey } from '@/api/admin/adminAccessKeys'

const { api, run } = vi.hoisted(() => ({
  api: { capabilities: vi.fn(), list: vi.fn(), create: vi.fn(), update: vi.fn(), change: vi.fn() },
  run: vi.fn((action: () => Promise<unknown>) => action())
}))
vi.mock('@/api/admin/adminAccessKeys', () => ({ adminAccessKeysAPI: api }))
vi.mock('@/composables/useStepUp', () => ({
  useStepUp: () => ({ run }), isStepUpCancelled: () => false
}))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key, te: () => false })
}))
const Dialog = defineComponent({
  props: { show: Boolean },
  template: '<div v-if="show" role="dialog"><slot /><slot name="footer" /></div>'
})
function create() {
  return mount(AdminAccessKeysSettings, {
    global: { stubs: { BaseDialog: Dialog, Icon: true, TotpStepUpDialog: true } }
  })
}
function findButton(wrapper: ReturnType<typeof create>, text: string) {
  const found = wrapper.findAll('button').find(node => node.text() === text)
  expect(found, text).toBeDefined()
  return found!
}
beforeEach(() => {
  vi.clearAllMocks()
  api.capabilities.mockResolvedValue({ enabled: true, permissions: ['accounts.read'] })
  api.list.mockResolvedValue({ items: [], next_before_id: 0 })
})
afterEach(() => vi.unstubAllGlobals())
describe('scoped access key settings', () => {
  it('hides the card and does not query key storage when disabled', async () => {
    api.capabilities.mockResolvedValue({ enabled: false, permissions: [] })
    const wrapper = create()
    await flushPromises()
    expect(wrapper.find('section').exists()).toBe(false)
    expect(api.list).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('creates through step-up and requires acknowledgement before clearing the one-time secret', async () => {
    vi.stubGlobal('navigator', { clipboard: { writeText: vi.fn().mockRejectedValue(new Error('private clipboard error')) } })
    api.create.mockResolvedValue({ secret: 'one-time-fixture-value' })
    const wrapper = create()
    await flushPromises()
    await findButton(wrapper, 'accessKeys.create').trigger('click')
    const fields = wrapper.findAll('#access-key-form input')
    await fields[0].setValue('Restricted automation')
    const all = wrapper.findAll('#access-key-form input[type="checkbox"]').find(node => node.element.parentElement?.textContent?.includes('accessKeys.all'))
    expect(all).toBeDefined()
    await all!.setValue(true)
    await wrapper.get('#access-key-form').trigger('submit')
    await flushPromises()
    expect(run).toHaveBeenCalledTimes(1)
    expect(api.create).toHaveBeenCalledWith(expect.objectContaining({
      name: 'Restricted automation', rpm: 60,
      policy: { rules: [expect.objectContaining({ permissions: ['accounts.read'], all_resources: true })] }
    }))
    expect(wrapper.text()).toContain('one-time-fixture-value')
    expect(findButton(wrapper, 'common.close').attributes('disabled')).toBeDefined()
    await findButton(wrapper, 'common.copy').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="dialog"] [role="alert"]').text()).toBe('accessKeys.copyFailed')
    expect(wrapper.text()).not.toContain('private clipboard error')
    await wrapper.get('[role="dialog"] input[type="checkbox"]').setValue(true)
    await findButton(wrapper, 'common.close').trigger('click')
    expect(wrapper.text()).not.toContain('one-time-fixture-value')
    wrapper.unmount()
  })
  it('preserves multi-rule policy and revision when editing', async () => {
    const key = {
      id: 4, name: 'Multi rule', prefix: 'fixture', description: '', enabled: true,
      rpm: 30, revision: 7, expires_at: '2030-11-03T06:30:45.123Z', ip_allowlist: [],
      policy: { rules: [
        { permissions: ['accounts.read'], account_ids: [12] },
        { permissions: ['groups.read'], group_ids: [34] }
      ] }
    } as unknown as AccessKey
    api.list.mockResolvedValue({ items: [key], next_before_id: 0 })
    api.update.mockResolvedValue(undefined)
    const wrapper = create()
    await flushPromises()
    await findButton(wrapper, 'common.edit').trigger('click')
    expect(wrapper.get('textarea[spellcheck="false"]').attributes('disabled')).toBeUndefined()
    await wrapper.get('#access-key-form').trigger('submit')
    await flushPromises()
    expect(api.update).toHaveBeenCalledWith(4, expect.objectContaining({
      revision: 7, policy: key.policy, expires_at: key.expires_at
    }))
    wrapper.unmount()
  })
  it('aborts a pending capabilities read when unmounted', async () => {
    let resolve!: (value: unknown) => void
    api.capabilities.mockImplementation(() => new Promise(done => { resolve = done }))
    const wrapper = create()
    wrapper.unmount()
    expect((api.capabilities.mock.calls[0][0] as AbortSignal).aborted).toBe(true)
    resolve({ enabled: true, permissions: [] })
    await flushPromises()
    expect(api.list).not.toHaveBeenCalled()
  })
  it('loads older keys and resets the cursor on refresh', async () => {
    const key = (id: number) => ({
      id, name: `Key ${id}`, prefix: 'fixture', description: '', enabled: true,
      rpm: 60, revision: 1, expires_at: null, ip_allowlist: [],
      policy: { rules: [{ permissions: ['accounts.read'], all_resources: true }] }
    })
    api.list.mockResolvedValueOnce({ items: [key(202)], next_before_id: 202 })
      .mockResolvedValueOnce({ items: [key(1)], next_before_id: 0 })
      .mockResolvedValueOnce({ items: [key(203)], next_before_id: 203 })
    const wrapper = create()
    await flushPromises()
    await findButton(wrapper, 'accessKeys.loadMore').trigger('click')
    await flushPromises()
    expect(api.list).toHaveBeenLastCalledWith(expect.any(AbortSignal), 202)
    expect(wrapper.findAll('article').map(n => n.find('h3').text())).toEqual(['Key 202', 'Key 1'])
    expect(wrapper.text()).not.toContain('accessKeys.loadMore')
    await findButton(wrapper, 'common.refresh').trigger('click')
    await flushPromises()
    expect(api.list).toHaveBeenLastCalledWith(expect.any(AbortSignal), 0)
    expect(wrapper.findAll('article').map(n => n.find('h3').text())).toEqual(['Key 203'])
    wrapper.unmount()
  })
})
