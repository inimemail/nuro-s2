import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { ChannelMonitor } from '@/api/admin/channelMonitor'
import MonitorActionsCell from '@/components/admin/monitor/MonitorActionsCell.vue'
import ChannelMonitorView from '@/views/admin/ChannelMonitorView.vue'

const { listMonitors, duplicateMonitor, showSuccess, showError } = vi.hoisted(() => ({
  listMonitors: vi.fn(),
  duplicateMonitor: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    channelMonitor: {
      list: listMonitors,
      duplicate: duplicateMonitor,
      update: vi.fn(),
      runNow: vi.fn(),
      del: vi.fn(),
    },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess, showError }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const AppLayoutStub = defineComponent({ template: '<main><slot /></main>' })
const TablePageLayoutStub = defineComponent({
  template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>',
})
const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] } },
  template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-actions" :row="row" /></div></div>',
})

function makeMonitor(overrides: Partial<ChannelMonitor> = {}): ChannelMonitor {
  return {
    id: 42,
    name: 'primary',
    provider: 'openai',
    api_mode: 'chat_completions',
    endpoint: 'https://api.example.com',
    api_key_masked: 'sk-t***',
    primary_model: 'gpt-4o-mini',
    extra_models: [],
    group_name: '',
    enabled: true,
    interval_seconds: 60,
    last_checked_at: null,
    created_by: 1,
    created_at: '2026-07-16T00:00:00Z',
    updated_at: '2026-07-16T00:00:00Z',
    primary_status: '',
    primary_latency_ms: null,
    availability_7d: 0,
    extra_models_status: [],
    template_id: null,
    extra_headers: {},
    body_override_mode: 'off',
    body_override: null,
    ...overrides,
  }
}

const monitor = makeMonitor()

function mountView() {
  return mount(ChannelMonitorView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        TablePageLayout: TablePageLayoutStub,
        DataTable: DataTableStub,
        MonitorFiltersBar: true,
        Pagination: true,
        ConfirmDialog: true,
        EmptyState: true,
        HelpTooltip: true,
        Toggle: true,
        MonitorFormDialog: true,
        MonitorTemplateManagerDialog: true,
        MonitorRunResultDialog: true,
        MonitorPrimaryModelCell: true,
      },
    },
  })
}

describe('ChannelMonitorView duplicate action', () => {
  beforeEach(() => {
    for (const fn of [listMonitors, duplicateMonitor, showSuccess, showError]) fn.mockReset()
    listMonitors.mockResolvedValue({ items: [monitor], total: 1, page: 1, page_size: 20 })
    duplicateMonitor.mockResolvedValue(makeMonitor({ id: 43, name: 'primary (Copy)', enabled: false }))
  })

  it('duplicates once, reports success and reloads the list', async () => {
    const wrapper = mountView()
    await flushPromises()

    wrapper.findComponent(MonitorActionsCell).vm.$emit('duplicate', monitor)
    await flushPromises()

    expect(duplicateMonitor).toHaveBeenCalledOnce()
    expect(duplicateMonitor).toHaveBeenCalledWith(42)
    expect(showSuccess).toHaveBeenCalledWith('admin.channelMonitor.duplicateSuccess')
    expect(listMonitors.mock.calls.length).toBeGreaterThan(1)
    wrapper.unmount()
  })

  it('ignores repeated events while the request is in flight', async () => {
    let resolveDuplicate!: (value: ChannelMonitor) => void
    duplicateMonitor.mockImplementationOnce(() => new Promise(resolve => { resolveDuplicate = resolve }))
    const wrapper = mountView()
    await flushPromises()

    const actions = wrapper.findComponent(MonitorActionsCell)
    actions.vm.$emit('duplicate', monitor)
    actions.vm.$emit('duplicate', monitor)
    await wrapper.vm.$nextTick()

    expect(duplicateMonitor).toHaveBeenCalledOnce()
    expect(actions.props('duplicating')).toBe(true)
    resolveDuplicate(makeMonitor({ id: 43, name: 'primary (Copy)', enabled: false }))
    await flushPromises()
    expect(wrapper.findComponent(MonitorActionsCell).props('duplicating')).toBe(false)
    wrapper.unmount()
  })

  it('does not call the API when the stored key is unavailable', async () => {
    const unavailable = makeMonitor({ id: 99, api_key_decrypt_failed: true })
    listMonitors.mockResolvedValueOnce({ items: [unavailable], total: 1, page: 1, page_size: 20 })
    const wrapper = mountView()
    await flushPromises()

    wrapper.findComponent(MonitorActionsCell).vm.$emit('duplicate', unavailable)
    await flushPromises()

    expect(duplicateMonitor).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.channelMonitor.duplicateKeyUnavailable')
    wrapper.unmount()
  })
})
