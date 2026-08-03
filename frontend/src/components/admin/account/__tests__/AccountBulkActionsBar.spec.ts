import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import AccountBulkActionsBar from '../AccountBulkActionsBar.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${JSON.stringify(params)}` : key
    })
  }
})

function mountBar(overrides: Partial<{
  selectedIds: number[]
  totalResults: number
  selectingAll: boolean
  allResultsSelected: boolean
}> = {}) {
  return mount(AccountBulkActionsBar, {
    props: {
      selectedIds: [],
      totalResults: 8,
      selectingAll: false,
      allResultsSelected: false,
      ...overrides
    },
    global: { stubs: { Icon: true } }
  })
}

describe('AccountBulkActionsBar', () => {
  it('offers all filtered results even before the current page is selected', async () => {
    const wrapper = mountBar()
    const button = wrapper.get('button')

    expect(wrapper.text()).toContain('admin.accounts.bulkActions.selectAllResults:{"count":8}')
    await button.trigger('click')
    expect(wrapper.emitted('select-all-results')).toHaveLength(1)
  })

  it('shows progress and disables duplicate select-all requests', () => {
    const wrapper = mountBar({ selectingAll: true })
    const selectAllButton = wrapper.findAll('button').find(button =>
      button.text().includes('admin.accounts.bulkActions.selectingAll')
    )

    expect(selectAllButton?.attributes('disabled')).toBeDefined()
  })

  it('shows the all-results state and hides the select-all command', () => {
    const wrapper = mountBar({ selectedIds: [1, 2, 3], totalResults: 3, allResultsSelected: true })

    expect(wrapper.text()).toContain('admin.accounts.bulkActions.selectedAll:{"count":3}')
    expect(wrapper.text()).not.toContain('admin.accounts.bulkActions.selectAllResults')
  })
})
