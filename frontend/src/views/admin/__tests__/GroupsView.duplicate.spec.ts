import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { AdminGroup } from '@/types'
import GroupsView from '@/views/admin/GroupsView.vue'

const { listGroups, duplicateGroup, getUsageSummary, getCapacitySummary, showSuccess, showError } = vi.hoisted(() => ({
  listGroups: vi.fn(),
  duplicateGroup: vi.fn(),
  getUsageSummary: vi.fn(),
  getCapacitySummary: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    groups: {
      list: listGroups,
      duplicate: duplicateGroup,
      getUsageSummary,
      getCapacitySummary,
      getModelsListCandidates: vi.fn().mockResolvedValue([]),
      getAll: vi.fn().mockResolvedValue([]),
      create: vi.fn(),
      update: vi.fn(),
      delete: vi.fn(),
      updateSortOrder: vi.fn()
    },
    accounts: {
      list: vi.fn().mockResolvedValue({ items: [] }),
      getById: vi.fn()
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess, showError })
}))

vi.mock('@/stores/onboarding', () => ({
  useOnboardingStore: () => ({ isCurrentStep: vi.fn(() => false), nextStep: vi.fn() })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const sourceGroup = {
  id: 42,
  name: 'Primary',
  platform: 'openai',
  status: 'active',
  rate_multiplier: 1,
  rpm_limit: 0,
  is_exclusive: false,
  subscription_type: 'standard',
  account_count: 1,
  active_account_count: 1,
  rate_limited_account_count: 0,
  sort_order: 10,
  created_at: '2026-07-16T00:00:00Z',
  updated_at: '2026-07-16T00:00:00Z'
} as unknown as AdminGroup

const DataTableStub = defineComponent({
  props: {
    data: { type: Array, default: () => [] },
    columns: { type: Array, default: () => [] },
    loading: { type: Boolean, default: false }
  },
  template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-actions" :row="row" /></div></div>'
})

function mountView() {
  return mount(GroupsView, {
    global: {
      stubs: {
        AppLayout: { template: '<main><slot /></main>' },
        TablePageLayout: { template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>' },
        DataTable: DataTableStub,
        Pagination: true,
        BaseDialog: true,
        ConfirmDialog: true,
        EmptyState: true,
        Select: true,
        PlatformIcon: true,
        Icon: true,
        GroupCapacityBadge: true,
        GroupRateMultipliersModal: true,
        GroupRPMOverridesModal: true,
        VueDraggable: true
      }
    }
  })
}

describe('GroupsView duplicate action', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.spyOn(console, 'error').mockImplementation(() => {})
    for (const fn of [listGroups, duplicateGroup, getUsageSummary, getCapacitySummary, showSuccess, showError]) {
      fn.mockReset()
    }
    listGroups.mockResolvedValue({ items: [sourceGroup], total: 1, page: 1, page_size: 20, pages: 1 })
    duplicateGroup.mockResolvedValue({ ...sourceGroup, id: 43, name: 'Primary (Copy)', status: 'inactive' })
    getUsageSummary.mockResolvedValue([])
    getCapacitySummary.mockResolvedValue([])
  })

  afterEach(() => vi.restoreAllMocks())

  it('duplicates the selected group and refreshes the list', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-testid="group-duplicate"]').trigger('click')
    await flushPromises()

    expect(duplicateGroup).toHaveBeenCalledWith(42)
    expect(showSuccess).toHaveBeenCalledWith('admin.groups.duplicateSuccess')
    expect(listGroups).toHaveBeenCalledTimes(2)
  })

  it('ignores repeated clicks while duplication is in flight', async () => {
    let resolveDuplicate!: (value: AdminGroup) => void
    duplicateGroup.mockImplementationOnce(() => new Promise<AdminGroup>((resolve) => { resolveDuplicate = resolve }))
    const wrapper = mountView()
    await flushPromises()

    const button = wrapper.get('[data-testid="group-duplicate"]')
    void button.trigger('click')
    void button.trigger('click')
    await wrapper.vm.$nextTick()
    expect(duplicateGroup).toHaveBeenCalledTimes(1)
    expect(button.attributes('disabled')).toBeDefined()
    expect(button.attributes('title')).toBe('admin.groups.duplicating')

    resolveDuplicate({ ...sourceGroup, id: 43 })
    await flushPromises()
    expect(wrapper.get('[data-testid="group-duplicate"]').attributes('disabled')).toBeUndefined()
  })

  it('restores the action and reports a duplicate failure', async () => {
    duplicateGroup.mockRejectedValueOnce(new Error('duplicate failed'))
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-testid="group-duplicate"]').trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('duplicate failed')
    expect(wrapper.get('[data-testid="group-duplicate"]').attributes('disabled')).toBeUndefined()
  })
})
