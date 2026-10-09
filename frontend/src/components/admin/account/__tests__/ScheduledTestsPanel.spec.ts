import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ScheduledTestsPanel from '../ScheduledTestsPanel.vue'

const api = vi.hoisted(() => ({ listByAccount: vi.fn(), create: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { scheduledTests: api } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/utils/format', () => ({ formatDateTime: (value: string) => value }))
const input = { props: ['modelValue'], emits: ['update:modelValue'], template: '<input :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />' }

describe('scheduled test drafts', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    api.listByAccount.mockResolvedValue([])
  })

  it('keeps a new plan draft typed while the previous plan is being created', async () => {
    let resolve!: () => void
    api.create.mockReturnValueOnce(new Promise<void>(done => { resolve = done }))
    const wrapper = mount(ScheduledTestsPanel, {
      props: { show: true, accountId: 1, modelOptions: [] },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' }, ConfirmDialog: true, Icon: true, HelpTooltip: true, Toggle: true, Select: input, Input: input } }
    })
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === 'admin.scheduledTests.addPlan')!.trigger('click')
    await wrapper.findAll('input')[0].setValue('submitted-model')
    await wrapper.findAll('input')[1].setValue('*/30 * * * *')
    await wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')
    await wrapper.findAll('input')[0].setValue('new-draft')
    resolve()
    await flushPromises()
    expect((wrapper.findAll('input')[0].element as HTMLInputElement).value).toBe('new-draft')
    expect(api.create).toHaveBeenCalledWith(expect.objectContaining({ account_id: 1, model_id: 'submitted-model' }))
    wrapper.unmount()
  })
})
