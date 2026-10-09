import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import GroupRateMultipliersModal from '../GroupRateMultipliersModal.vue'
import GroupRPMOverridesModal from '../GroupRPMOverridesModal.vue'
import type { AdminGroup } from '@/types'

const api = vi.hoisted(() => ({
  getGroupRateMultipliers: vi.fn(), getGroupRPMOverrides: vi.fn(),
  batchSetGroupRateMultipliers: vi.fn(), batchSetGroupRPMOverrides: vi.fn(),
  clearGroupRPMOverrides: vi.fn(), users: vi.fn()
}))
vi.mock('@/api/admin', () => ({ adminAPI: { groups: api, users: { list: api.users } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const group = (id: number) => ({ id, name: `group-${id}`, platform: 'openai', rate_multiplier: 1 } as AdminGroup)
const entry = (email: string) => ({ user_id: 2, user_name: 'Client', user_email: email, user_status: 'active', rate_multiplier: 0.75, rpm_override: 1000000 })
const deferred = <T,>() => {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(done => { resolve = done })
  return { promise, resolve }
}

describe.each([
  { name: 'rates', component: GroupRateMultipliersModal, load: api.getGroupRateMultipliers, save: api.batchSetGroupRateMultipliers },
  { name: 'RPM', component: GroupRPMOverridesModal, load: api.getGroupRPMOverrides, save: api.batchSetGroupRPMOverrides }
])('$name override dialog lifecycle', ({ component, load, save }) => {
  beforeEach(() => {
    vi.clearAllMocks()
    load.mockReset()
    save.mockReset().mockResolvedValue(undefined)
  })

  const mountDialog = () => mount(component, {
    props: { show: true, group: group(1) },
    global: { stubs: { BaseDialog: { template: '<div><slot /></div>' }, Pagination: true, Icon: true, PlatformIcon: true } }
  })

  it('rejects the old group response after switching groups', async () => {
    const old = deferred<ReturnType<typeof entry>[]>()
    load.mockReturnValueOnce(old.promise).mockResolvedValueOnce([entry('current@example.test')])
    const wrapper = mountDialog()
    await wrapper.setProps({ group: group(2) })
    await flushPromises()
    old.resolve([entry('stale@example.test')])
    await flushPromises()
    expect(wrapper.text()).toContain('current@example.test')
    expect(wrapper.text()).not.toContain('stale@example.test')
    wrapper.unmount()
  })

  it('keeps saving blocked when the initial load failed', async () => {
    load.mockRejectedValueOnce(new Error('offline'))
    const wrapper = mountDialog()
    await flushPromises()
    expect(wrapper.findAll('button').filter(button => button.text() === 'common.save')).toHaveLength(0)
    expect(save).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('preserves a dirty draft when the same group object is refreshed', async () => {
    load.mockResolvedValue([entry('client@example.test')])
    const wrapper = mountDialog()
    await flushPromises()
    await wrapper.find('tbody input[type="number"]').setValue('2')
    await wrapper.setProps({ group: { ...group(1), name: 'refreshed' } })
    await flushPromises()
    expect(load).toHaveBeenCalledOnce()
    expect((wrapper.find('tbody input[type="number"]').element as HTMLInputElement).value).toBe('2')
    wrapper.unmount()
  })

  it('keeps a newer draft open after the submitted values are saved', async () => {
    load.mockResolvedValue([entry('client@example.test')])
    const pending = deferred<void>()
    save.mockReturnValueOnce(pending.promise)
    const wrapper = mountDialog()
    await flushPromises()
    await wrapper.find('tbody input[type="number"]').setValue('2')
    await wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')
    await wrapper.find('tbody input[type="number"]').setValue('3')
    pending.resolve()
    await flushPromises()
    expect(wrapper.emitted('success')).toHaveLength(1)
    expect(wrapper.emitted('close')).toBeUndefined()
    expect((wrapper.find('tbody input[type="number"]').element as HTMLInputElement).value).toBe('3')
    expect(wrapper.findAll('button').find(button => button.text() === 'common.save')).toBeDefined()
    wrapper.unmount()
  })

  it('does not close a reopened dialog when the previous save finishes', async () => {
    load.mockResolvedValue([entry('client@example.test')])
    const pending = deferred<void>()
    save.mockReturnValueOnce(pending.promise)
    const wrapper = mountDialog()
    await flushPromises()
    const numeric = wrapper.find('tbody input[type="number"]')
    await numeric.setValue('2')
    const saveButton = wrapper.findAll('button').find(button => button.text() === 'common.save')!
    await saveButton.trigger('click')
    expect(save).toHaveBeenCalledOnce()
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()
    pending.resolve()
    await flushPromises()
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.emitted('success')).toBeUndefined()
    wrapper.unmount()
  })

  if (component === GroupRPMOverridesModal) {
    it('preserves edits made while clearing stored overrides', async () => {
      load.mockResolvedValue([entry('client@example.test')])
      const pending = deferred<void>()
      api.clearGroupRPMOverrides.mockReturnValueOnce(pending.promise)
      const wrapper = mountDialog()
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text() === 'admin.groups.clearAll')!.trigger('click')
      await wrapper.find('tbody input[type="number"]').setValue('3')
      pending.resolve()
      await flushPromises()
      expect((wrapper.find('tbody input[type="number"]').element as HTMLInputElement).value).toBe('3')
      expect(wrapper.emitted('success')).toHaveLength(1)
      expect(wrapper.findAll('button').find(button => button.text() === 'common.save')).toBeDefined()
      wrapper.unmount()
    })

    it('rejects fractional, empty and unsafe RPM edits', async () => {
      load.mockResolvedValue([entry('client@example.test')])
      const wrapper = mountDialog()
      await flushPromises()
      for (const value of ['2.5', '', '9007199254740992', '-1']) {
        await wrapper.find('tbody input[type="number"]').setValue(value)
        expect((wrapper.find('tbody input[type="number"]').element as HTMLInputElement).value).toBe('1000000')
        expect(wrapper.findAll('button').filter(button => button.text() === 'common.save')).toHaveLength(0)
      }
      await wrapper.find('tbody input[type="number"]').setValue('0')
      await wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')
      expect(save).toHaveBeenCalledWith(1, [{ user_id: 2, rpm_override: 0 }])
      wrapper.unmount()
    })
  }
})
