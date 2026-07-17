import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import DataTable from '../DataTable.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

describe('DataTable selection', () => {
  beforeEach(() => {
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockReturnValue({
        matches: true,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn()
      })
    })
  })

  it('emits stable row keys for row and select-all changes', async () => {
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: [{ id: 11, name: 'A' }, { id: 12, name: 'B' }],
        rowKey: 'id',
        selectable: true,
        selectedKeys: []
      }
    })

    const checkboxes = wrapper.findAll('input[type="checkbox"]')
    await checkboxes[1].setValue(true)
    expect(wrapper.emitted('update:selectedKeys')?.[0]).toEqual([[11]])

    await checkboxes[0].setValue(true)
    expect(wrapper.emitted('update:selectedKeys')?.[1]).toEqual([[11, 12]])
  })

  it('uses the extra selection column in the empty table colspan', () => {
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: [],
        selectable: true
      }
    })
    expect(wrapper.get('tbody td').attributes('colspan')).toBe('2')
  })

  it('keeps the selection column and first data column fixed during horizontal scroll', () => {
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }, { key: 'details', label: 'Details' }],
        data: [{ id: 11, name: 'A', details: 'Long details' }],
        rowKey: 'id',
        selectable: true,
        stickyFirstColumn: true
      }
    })

    expect(wrapper.get('thead th').classes()).toContain('sticky-col-left-first')
    const cells = wrapper.findAll('tbody tr td')
    expect(cells[0].classes()).toContain('sticky-col-left-first')
    expect(cells[1].classes()).toContain('sticky-col-left-second')
  })
})
