import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'

const {
  listAccounts,
  listWithEtag,
  getBatchTodayStats,
  getAllProxies,
  getAllGroups,
  getUpstreamBillingProbeSettings,
  updateUpstreamBillingProbeSettings,
  batchDelete,
  showError,
  showSuccess
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  updateUpstreamBillingProbeSettings: vi.fn(),
  batchDelete: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag,
      getBatchTodayStats,
      batchDelete,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
			toggleSchedulable: vi.fn(),
			getUpstreamBillingProbeSettings,
			updateUpstreamBillingProbeSettings
    },
    proxies: {
      getAll: getAllProxies
    },
    groups: {
      getAll: getAllGroups
    },
    settings: {
      getSettings: vi.fn().mockResolvedValue({})
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    token: 'test-token'
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const DataTableStub = {
  props: ['columns', 'data'],
  template: `
    <div data-test="data-table">
      <span v-for="column in columns" :key="column.key" data-test="column-key">{{ column.key }}</span>
      <div v-for="row in data" :key="row.id">
        <slot name="cell-created_at" :value="row.created_at" :row="row" />
        <slot name="cell-prompt_cache_boost" :row="row" />
      </div>
    </div>
  `
}

const AccountBulkActionsBarStub = {
  props: ['selectedIds', 'totalResults', 'selectingAll', 'allResultsSelected'],
  emits: [
    'delete',
    'edit-selected',
    'edit-filtered',
    'clear',
    'select-page',
    'select-all-results',
    'toggle-schedulable',
    'reset-status',
    'refresh-token'
  ],
  template: `
    <div>
      <button data-test="edit-filtered" @click="$emit('edit-filtered')">edit filtered</button>
      <button data-test="select-all-results" @click="$emit('select-all-results')">select all</button>
      <button data-test="bulk-delete" @click="$emit('delete')">delete</button>
    </div>
  `
}

const BulkEditAccountModalStub = {
  props: ['show', 'target'],
  template: '<div data-test="bulk-edit-modal" :data-show="String(show)" :data-target-mode="target?.mode ?? \'\'"></div>'
}

const accountViewStubs = {
  AppLayout: { template: '<div><slot /></div>' },
  TablePageLayout: {
    template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
  },
  DataTable: DataTableStub,
  Pagination: true,
  ConfirmDialog: true,
  AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
  AccountTableFilters: { template: '<div></div>' },
  AccountBulkActionsBar: AccountBulkActionsBarStub,
  AccountActionMenu: true,
  ImportDataModal: true,
  ReAuthAccountModal: true,
  AccountTestModal: true,
  AccountStatsModal: true,
  ScheduledTestsPanel: true,
  SyncFromCrsModal: true,
  TempUnschedStatusModal: true,
  ErrorPassthroughRulesModal: true,
  TLSFingerprintProfilesModal: true,
  CreateAccountModal: true,
  EditAccountModal: true,
  BulkEditAccountModal: BulkEditAccountModalStub,
  PlatformTypeBadge: true,
  AccountCapacityCell: true,
  AccountStatusIndicator: true,
  AccountTodayStatsCell: true,
  AccountGroupsCell: true,
  AccountUsageCell: true,
  Icon: true
}

function mountAccountsView() {
  return mount(AccountsView, {
    global: {
      stubs: accountViewStubs
    }
  })
}

describe('admin AccountsView bulk edit scope', () => {
  beforeEach(() => {
    localStorage.clear()

    listAccounts.mockReset()
    listWithEtag.mockReset()
    getBatchTodayStats.mockReset()
    getAllProxies.mockReset()
    getAllGroups.mockReset()
    getUpstreamBillingProbeSettings.mockReset()
    updateUpstreamBillingProbeSettings.mockReset()
    batchDelete.mockReset()
    showError.mockReset()
    showSuccess.mockReset()

    listAccounts.mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      page_size: 20,
      pages: 0
    })
    listWithEtag.mockResolvedValue({
      notModified: true,
      etag: null,
      data: null
    })
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getAllProxies.mockResolvedValue([])
    getAllGroups.mockResolvedValue([])
    getUpstreamBillingProbeSettings.mockResolvedValue({ enabled: false, interval_seconds: 5 })
    updateUpstreamBillingProbeSettings.mockImplementation(async settings => settings)
    batchDelete.mockResolvedValue({ total: 0, success: 0, failed: 0 })
  })

  it('opens bulk edit in filtered-results mode from the bulk actions dropdown', async () => {
    const wrapper = mountAccountsView()

    await flushPromises()
    await wrapper.get('[data-test="edit-filtered"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="bulk-edit-modal"]').attributes('data-show')).toBe('true')
    expect(wrapper.get('[data-test="bulk-edit-modal"]').attributes('data-target-mode')).toBe('filtered')
  })

  it('selects every filtered result and retains only failed IDs after batch deletion', async () => {
    const items = [
      {
        id: 1,
        name: 'account-one',
        platform: 'openai',
        type: 'apikey',
        status: 'active',
        schedulable: true,
        created_at: '2026-08-03T00:00:00Z',
        updated_at: '2026-08-03T00:00:00Z'
      },
      {
        id: 2,
        name: 'account-two',
        platform: 'grok',
        type: 'apikey',
        status: 'active',
        schedulable: true,
        created_at: '2026-08-03T00:00:00Z',
        updated_at: '2026-08-03T00:00:00Z'
      }
    ]
    listAccounts.mockResolvedValue({
      items,
      total: 2,
      page: 1,
      page_size: 20,
      pages: 1
    })
    batchDelete.mockResolvedValue({
      total: 2,
      success: 1,
      failed: 1,
      success_ids: [1],
      failed_ids: [2],
      errors: [{ account_id: 2, error: 'account is still in use' }]
    })
    vi.stubGlobal('confirm', vi.fn(() => true))

    const wrapper = mountAccountsView()
    await flushPromises()
    await wrapper.get('[data-test="select-all-results"]').trigger('click')
    await flushPromises()

    const bulkBar = wrapper.getComponent(AccountBulkActionsBarStub)
    expect(bulkBar.props('selectedIds')).toEqual([1, 2])
    expect(bulkBar.props('allResultsSelected')).toBe(true)

    await wrapper.get('[data-test="bulk-delete"]').trigger('click')
    await flushPromises()

    expect(batchDelete).toHaveBeenCalledWith([1, 2])
    expect(bulkBar.props('selectedIds')).toEqual([2])
    expect(showError).toHaveBeenCalledWith('admin.accounts.bulkActions.deleteFailureDetails', 10000)
  })

  it('invalidates the all-results label when a refresh changes the result count', async () => {
    const items = [
      { id: 1, name: 'one', platform: 'openai', type: 'apikey', status: 'active', schedulable: true },
      { id: 2, name: 'two', platform: 'grok', type: 'apikey', status: 'active', schedulable: true }
    ]
    listAccounts.mockResolvedValue({ items, total: 2, page: 1, page_size: 20, pages: 1 })

    const wrapper = mountAccountsView()
    await flushPromises()
    await wrapper.get('[data-test="select-all-results"]').trigger('click')
    await flushPromises()

    const bulkBar = wrapper.getComponent(AccountBulkActionsBarStub)
    expect(bulkBar.props('allResultsSelected')).toBe(true)

    listAccounts.mockResolvedValue({
      items: [...items, { id: 3, name: 'three', platform: 'gemini', type: 'apikey', status: 'active', schedulable: true }],
      total: 3,
      page: 1,
      page_size: 20,
      pages: 1
    })
    await (wrapper.vm as unknown as { reload: () => Promise<void> }).reload()
    await flushPromises()

    expect(bulkBar.props('selectedIds')).toEqual([1, 2])
    expect(bulkBar.props('allResultsSelected')).toBe(false)
  })

  it('renders the created_at column by default', async () => {
    listAccounts.mockResolvedValue({
      items: [
        {
          id: 1,
          name: 'test-account',
          platform: 'anthropic',
          type: 'oauth',
          status: 'active',
          schedulable: true,
          created_at: '2026-03-07T10:00:00Z',
          updated_at: '2026-03-07T10:00:00Z'
        }
      ],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })

    const wrapper = mountAccountsView()

    await flushPromises()

    const columnKeys = wrapper.findAll('[data-test="column-key"]').map(node => node.text())
    expect(columnKeys).toContain('created_at')
    const columns = wrapper.getComponent(DataTableStub).props('columns') as Array<{ key: string; label: string; sortable: boolean }>
    expect(columns.find(column => column.key === 'created_at')).toMatchObject({
      label: 'admin.accounts.columns.createdAt',
      sortable: true
    })
  })

  it('renders Anthropic cache boost status from Anthropic credentials', async () => {
    listAccounts.mockResolvedValue({
      items: [
        {
          id: 2,
          name: 'anthropic-key-pool',
          platform: 'anthropic',
          type: 'apikey',
          status: 'active',
          schedulable: true,
          credentials: {
            pool_mode: true,
            anthropic_cache_boost_enabled: true,
            anthropic_upstream_strong_isolation_enabled: true
          },
          created_at: '2026-03-07T10:00:00Z',
          updated_at: '2026-03-07T10:00:00Z'
        },
        {
          id: 3,
          name: 'anthropic-oauth',
          platform: 'anthropic',
          type: 'oauth',
          status: 'active',
          schedulable: true,
          credentials: {
            anthropic_cache_boost_enabled: true,
            anthropic_upstream_strong_isolation_enabled: true
          },
          created_at: '2026-03-07T10:00:00Z',
          updated_at: '2026-03-07T10:00:00Z'
        },
        {
          id: 4,
          name: 'anthropic-key-non-pool',
          platform: 'anthropic',
          type: 'apikey',
          status: 'active',
          schedulable: true,
          credentials: {
            anthropic_cache_boost_enabled: true,
            anthropic_upstream_strong_isolation_enabled: true
          },
          created_at: '2026-03-07T10:00:00Z',
          updated_at: '2026-03-07T10:00:00Z'
        },
        {
          id: 5,
          name: 'anthropic-key-pool-disabled',
          platform: 'anthropic',
          type: 'apikey',
          status: 'active',
          schedulable: true,
          credentials: {
            pool_mode: true
          },
          created_at: '2026-03-07T10:00:00Z',
          updated_at: '2026-03-07T10:00:00Z'
        }
      ],
      total: 4,
      page: 1,
      page_size: 20,
      pages: 1
    })

    const wrapper = mountAccountsView()

    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.promptCacheBoostEnabled')
    expect(wrapper.text()).toContain('admin.accounts.upstreamStrongIsolationEnabled')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheBoostDisabled')
    expect(wrapper.text()).toContain('admin.accounts.upstreamStrongIsolationDisabled')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheBoostNotApplicable')
    expect(wrapper.text()).toContain('admin.accounts.upstreamStrongIsolationNotApplicable')
    expect(wrapper.html()).toContain('admin.accounts.anthropicCacheBoostEnabledHint')
    expect(wrapper.html()).toContain('admin.accounts.anthropicUpstreamStrongIsolationEnabledHint')
  })

	it('clamps the global upstream billing probe interval to 1-36000 seconds', async () => {
		const wrapper = mountAccountsView()
		await flushPromises()
		await wrapper.get('[title="admin.accounts.moreActions"]').trigger('click')

		const input = wrapper.get('[data-testid="upstream-billing-global-interval"]')
		await input.setValue('0')
		await wrapper.get('[data-testid="upstream-billing-global-save"]').trigger('click')
		await flushPromises()
		expect(updateUpstreamBillingProbeSettings).toHaveBeenLastCalledWith({
			enabled: false,
			interval_seconds: 1
		})

		await input.setValue('50000')
		await wrapper.get('[data-testid="upstream-billing-global-save"]').trigger('click')
		await flushPromises()
		expect(updateUpstreamBillingProbeSettings).toHaveBeenLastCalledWith({
			enabled: false,
			interval_seconds: 36000
		})
	})
})
