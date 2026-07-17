import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import BulkEditUserModal from '../BulkEditUserModal.vue'

const { batchUpdateLimits, showSuccess, showError } = vi.hoisted(() => ({
  batchUpdateLimits: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: { users: { batchUpdateLimits } }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess, showError })
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

const mountModal = (selectedIds: number[] = [4, 7]) => mount(BulkEditUserModal, {
  props: { show: true, selectedIds },
  global: {
    stubs: {
      BaseDialog: {
        props: ['show'],
        template: '<div v-if="show"><slot /><slot name="footer" /></div>'
      }
    }
  }
})

describe('BulkEditUserModal', () => {
  beforeEach(() => {
    batchUpdateLimits.mockReset()
    showSuccess.mockReset()
    showError.mockReset()
    batchUpdateLimits.mockResolvedValue({ affected: 2 })
  })

  afterEach(() => vi.restoreAllMocks())

  it('preserves explicit zero and only submits enabled selected-user fields', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const wrapper = mountModal()
    await wrapper.get('[data-test="enable-rpm-limit"]').trigger('click')
    await wrapper.get('[data-test="rpm-limit-input"]').setValue('0')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(batchUpdateLimits).toHaveBeenCalledWith({
      user_ids: [4, 7],
      all: false,
      rpm_limit: 0
    })
  })

  it('supports all users without requiring a selection', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const wrapper = mountModal([])
    expect((wrapper.get('[data-test="scope-all"]').element as HTMLInputElement).checked).toBe(true)
    await wrapper.get('[data-test="enable-concurrency"]').trigger('click')
    await wrapper.get('[data-test="concurrency-input"]').setValue('6')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(batchUpdateLimits).toHaveBeenCalledWith({
      user_ids: [],
      all: true,
      concurrency: 6
    })
    expect(window.confirm).toHaveBeenCalledWith('admin.users.bulkLimits.confirmAll')
  })

  it('limits only selected scope to 500 users', async () => {
    const wrapper = mountModal(Array.from({ length: 501 }, (_, index) => index + 1))
    await wrapper.get('[data-test="enable-concurrency"]').trigger('click')
    await wrapper.get('[data-test="concurrency-input"]').setValue('5')
    expect(wrapper.get('[data-test="submit"]').attributes('disabled')).toBeDefined()

    await wrapper.get('[data-test="scope-all"]').setValue(true)
    expect(wrapper.get('[data-test="submit"]').attributes('disabled')).toBeUndefined()
  })
})
