import { describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

const { updateAccountMock, checkMixedChannelRiskMock, authStoreMock } = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn(),
  authStoreMock: { isSimpleMode: true }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => authStoreMock
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      checkMixedChannelRisk: checkMixedChannelRiskMock
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([])
    }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
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

import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: {
      type: Boolean,
      default: false
    }
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => []
    }
  },
  emits: ['update:modelValue'],
  template: `
    <div>
      <button
        type="button"
        data-testid="rewrite-to-snapshot"
        @click="$emit('update:modelValue', ['gpt-5.2-2025-12-11'])"
      >
        rewrite
      </button>
      <span data-testid="model-whitelist-value">
        {{ Array.isArray(modelValue) ? modelValue.join(',') : '' }}
      </span>
    </div>
  `
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: {
      type: [String, Number, Boolean, null],
      default: ''
    },
    options: {
      type: Array,
      default: () => []
    }
  },
  emits: ['update:modelValue'],
  template: `
    <select
      v-bind="$attrs"
      :value="modelValue"
      @change="$emit('update:modelValue', $event.target.value)"
    >
      <option v-for="option in options" :key="option.value" :value="option.value">
        {{ option.label }}
      </option>
    </select>
  `
})

const GroupSelectorStub = defineComponent({
  name: 'GroupSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => []
    }
  },
  emits: ['update:modelValue'],
  template: `
    <button
      type="button"
      data-testid="remove-last-group"
      @click="$emit('update:modelValue', modelValue.slice(0, -1))"
    >
      remove
    </button>
  `
})

function buildGroup(id: number, name: string) {
  return { id, name, platform: 'openai', status: 'active' } as any
}

function buildAccount() {
  return {
    id: 1,
    name: 'OpenAI Key',
    notes: '',
    platform: 'openai',
    type: 'apikey',
    credentials: {
      api_key: 'sk-test',
      base_url: 'https://api.openai.com',
      model_mapping: {
        'gpt-5.2': 'gpt-5.2'
      }
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function buildVertexAccount() {
  return {
    id: 2,
    name: 'Vertex SA',
    notes: '',
    platform: 'gemini',
    type: 'service_account',
    credentials: {
      service_account_json: '{"type":"service_account","client_email":"sa@example.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\\nMIIE\\n-----END PRIVATE KEY-----\\n"}',
      project_id: 'demo-project',
      client_email: 'sa@example.iam.gserviceaccount.com',
      location: 'us-central1',
      tier_id: 'vertex'
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function buildOpenAIOAuthAccount() {
  return {
    id: 3,
    name: 'OpenAI OAuth',
    notes: '',
    platform: 'openai',
    type: 'oauth',
    credentials: {
      access_token: 'oauth-token',
      model_mapping: {
        'gpt-5.2': 'gpt-5.2'
      },
      prompt_cache_boost_enabled: true,
      prompt_cache_boost_level: 'aggressive',
      prompt_cache_boost_upstream_hit_priority_enabled: true,
      prompt_cache_smart_routing_enabled: true,
      prompt_cache_account_relay_enabled: true,
      prompt_cache_key_optimization_enabled: true,
      prompt_cache_long_context_enhancement_enabled: true,
      openai_prompt_cache_creation_optimization_enabled: true,
      openai_prompt_cache_creation_optimization_mode: 'suppress',
      upstream_strong_isolation_enabled: true
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function buildAnthropicOAuthAccount() {
  return {
    id: 4,
    name: 'Anthropic OAuth',
    notes: '',
    platform: 'anthropic',
    type: 'oauth',
    credentials: {
      access_token: 'claude-token',
      anthropic_cache_boost_enabled: true,
      anthropic_cache_boost_level: 'aggressive',
      anthropic_cache_boost_upstream_hit_priority_enabled: true,
      anthropic_upstream_strong_isolation_enabled: true
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function buildAnthropicAPIKeyAccount(poolMode = false) {
  return {
    id: 5,
    name: 'Anthropic Key',
    notes: '',
    platform: 'anthropic',
    type: 'apikey',
    credentials: {
      api_key: 'sk-ant-test',
      base_url: 'https://api.anthropic.com',
      ...(poolMode ? { pool_mode: true } : {}),
      anthropic_cache_boost_enabled: true,
      anthropic_cache_boost_level: 'aggressive',
      anthropic_cache_boost_upstream_hit_priority_enabled: true,
      anthropic_upstream_strong_isolation_enabled: true
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function mountModal(account = buildAccount(), groups: any[] = [], simpleMode = true) {
  authStoreMock.isSimpleMode = simpleMode
  return mount(EditAccountModal, {
    props: {
      show: true,
      account,
      proxies: [],
      groups
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: SelectStub,
        Icon: true,
        ProxySelector: true,
        GroupSelector: GroupSelectorStub,
        ModelWhitelistSelector: ModelWhitelistSelectorStub
      }
    }
  })
}

describe('EditAccountModal', () => {
  it('renders prompt cache boost and upstream isolation controls for OpenAI OAuth accounts', async () => {
    const wrapper = mountModal(buildOpenAIOAuthAccount())

    expect(wrapper.text()).toContain('admin.accounts.promptCacheBoost')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheBoostAggressive')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheBoostUpstreamHitPriority')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheSmartRouting')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheAccountRelay')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheKeyOptimization')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheLongContextEnhancement')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheCreationOptimization')
    expect(wrapper.text()).toContain('admin.accounts.upstreamStrongIsolation')
  })

  it('submits prompt cache boost and upstream isolation settings for OpenAI OAuth accounts', async () => {
    const account = buildOpenAIOAuthAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.prompt_cache_boost_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.prompt_cache_boost_level).toBe('aggressive')
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.prompt_cache_boost_upstream_hit_priority_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.prompt_cache_smart_routing_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.prompt_cache_account_relay_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.prompt_cache_key_optimization_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.prompt_cache_long_context_enhancement_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.openai_prompt_cache_creation_optimization_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.openai_prompt_cache_creation_optimization_mode).toBe('suppress')
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.upstream_strong_isolation_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).not.toHaveProperty('pool_mode')
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).not.toHaveProperty('pool_soft_cooldown_enabled')
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).not.toHaveProperty('image_pool_mode')
  })

  it('shows and submits cache creation optimization for a non-pool OpenAI API Key account', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    expect(wrapper.text()).toContain('admin.accounts.promptCacheCreationOptimization')
    expect(wrapper.text()).not.toContain('admin.accounts.promptCacheBoostHint')

    await wrapper.get('[data-testid="prompt-cache-creation-optimization-toggle"]').trigger('click')
    expect(wrapper.get('[data-testid="prompt-cache-creation-mode-reduce"]').attributes('aria-pressed')).toBe('true')
    await wrapper.get('[data-testid="prompt-cache-creation-mode-suppress"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    const credentials = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(credentials?.openai_prompt_cache_creation_optimization_enabled).toBe(true)
    expect(credentials?.openai_prompt_cache_creation_optimization_mode).toBe('suppress')
    expect(credentials).not.toHaveProperty('pool_mode')
  })

  it('loads and submits protection limits from account-group bindings', async () => {
    const account = buildAccount()
    const groups = [buildGroup(10, 'Primary'), buildGroup(20, 'Backup')]
    account.group_ids = [10, 20]
    account.account_groups = [
      { account_id: account.id, group_id: 10, priority: 1, upstream_billing_guard_max_multiplier: 1 },
      { account_id: account.id, group_id: 20, priority: 2, upstream_billing_guard_max_multiplier: null }
    ]
    account.extra = { upstream_billing_probe_enabled: true }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account, groups)
    expect((wrapper.get('[data-testid="upstream-billing-guard-group-10"]').element as HTMLInputElement).value).toBe('1')
    expect((wrapper.get('[data-testid="upstream-billing-guard-group-20"]').element as HTMLInputElement).value).toBe('')
    await wrapper.get('[data-testid="upstream-billing-guard-group-10"]').setValue('3')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.upstream_billing_probe_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.upstream_billing_guard_group_limits).toEqual({ 10: 3 })
  })

  it('does not submit unchanged group protection during an unrelated account edit', async () => {
    const account = buildAccount()
    const groups = [buildGroup(10, 'Primary')]
    account.group_ids = [10]
    account.account_groups = [
      { account_id: account.id, group_id: 10, priority: 1, upstream_billing_guard_max_multiplier: 1 }
    ]
    account.extra = { upstream_billing_probe_enabled: false }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account, groups)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(updateAccountMock.mock.calls[0]?.[1]).not.toHaveProperty('upstream_billing_guard_group_limits')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.upstream_billing_probe_enabled).toBe(false)
  })

  it('omits blank group limits so blank means unrestricted', async () => {
    const account = buildAccount()
    const groups = [buildGroup(10, 'Primary')]
    account.group_ids = [10]
    account.account_groups = [
      { account_id: account.id, group_id: 10, priority: 1, upstream_billing_guard_max_multiplier: 1 }
    ]
    account.extra = { upstream_billing_probe_enabled: true }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account, groups)
    await wrapper.get('[data-testid="upstream-billing-guard-group-10"]').setValue('')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(updateAccountMock.mock.calls[0]?.[1]?.upstream_billing_guard_group_limits).toEqual({})
  })

  it('removes a group protection value when the account group is removed', async () => {
    const account = buildAccount()
    const groups = [buildGroup(10, 'Primary'), buildGroup(20, 'Backup')]
    account.group_ids = [10, 20]
    account.account_groups = [
      { account_id: account.id, group_id: 10, priority: 1, upstream_billing_guard_max_multiplier: 1 },
      { account_id: account.id, group_id: 20, priority: 2, upstream_billing_guard_max_multiplier: 2 }
    ]
    account.extra = { upstream_billing_probe_enabled: true }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account, groups, false)
    await wrapper.get('[data-testid="remove-last-group"]').trigger('click')
    expect(wrapper.find('[data-testid="upstream-billing-guard-group-20"]').exists()).toBe(false)
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(updateAccountMock.mock.calls[0]?.[1]?.group_ids).toEqual([10])
    expect(updateAccountMock.mock.calls[0]?.[1]?.upstream_billing_guard_group_limits).toEqual({ 10: 1 })
  })

  it('clears all group limits when protection is disabled', async () => {
    const account = buildAccount()
    const groups = [buildGroup(10, 'Primary')]
    account.group_ids = [10]
    account.account_groups = [
      { account_id: account.id, group_id: 10, priority: 1, upstream_billing_guard_max_multiplier: 1 }
    ]
    account.extra = { upstream_billing_probe_enabled: true }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account, groups)
    await wrapper.get('[data-testid="upstream-billing-guard-toggle"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.upstream_billing_probe_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.upstream_billing_guard_group_limits).toEqual({})
  })

  it('enables automatic probing when group protection is enabled', async () => {
    const account = buildAccount()
    const groups = [buildGroup(10, 'Primary')]
    account.group_ids = [10]
    account.account_groups = []
    account.extra = { upstream_billing_probe_enabled: false }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account, groups)
    await wrapper.get('[data-testid="upstream-billing-guard-toggle"]').trigger('click')
    await wrapper.get('[data-testid="upstream-billing-guard-group-10"]').setValue('1.5')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.upstream_billing_probe_enabled).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.upstream_billing_guard_group_limits).toEqual({ 10: 1.5 })
  })

  it('clears group protection when its required automatic probe is disabled', async () => {
    const account = buildAccount()
    const groups = [buildGroup(10, 'Primary')]
    account.group_ids = [10]
    account.account_groups = [
      { account_id: account.id, group_id: 10, priority: 1, upstream_billing_guard_max_multiplier: 1 }
    ]
    account.extra = { upstream_billing_probe_enabled: true }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account, groups)
    await wrapper.get('[data-testid="upstream-billing-auto-probe-toggle"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.upstream_billing_probe_enabled).toBe(false)
    expect(updateAccountMock.mock.calls[0]?.[1]?.upstream_billing_guard_group_limits).toEqual({})
  })

  it('removes both cache creation optimization credentials when the switch is turned off', async () => {
    const account = buildAccount()
    account.credentials.openai_prompt_cache_creation_optimization_enabled = true
    account.credentials.openai_prompt_cache_creation_optimization_mode = 'suppress'
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    expect(wrapper.get('[data-testid="prompt-cache-creation-optimization-toggle"]').attributes('aria-pressed')).toBe('true')
    await wrapper.get('[data-testid="prompt-cache-creation-optimization-toggle"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    const credentials = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(credentials).not.toHaveProperty('openai_prompt_cache_creation_optimization_enabled')
    expect(credentials).not.toHaveProperty('openai_prompt_cache_creation_optimization_mode')
  })

  it('resets cache creation optimization when switching from an enabled account to a disabled account', async () => {
    const enabledAccount = buildOpenAIOAuthAccount()
    const disabledAccount = buildAccount()
    disabledAccount.id = 6
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(disabledAccount)

    const wrapper = mountModal(enabledAccount)
    expect(wrapper.get('[data-testid="prompt-cache-creation-optimization-toggle"]').attributes('aria-pressed')).toBe('true')
    expect(wrapper.get('[data-testid="prompt-cache-creation-mode-suppress"]').attributes('aria-pressed')).toBe('true')

    await wrapper.setProps({ account: disabledAccount })
    await flushPromises()

    expect(wrapper.get('[data-testid="prompt-cache-creation-optimization-toggle"]').attributes('aria-pressed')).toBe('false')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    const credentials = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(credentials).not.toHaveProperty('openai_prompt_cache_creation_optimization_enabled')
    expect(credentials).not.toHaveProperty('openai_prompt_cache_creation_optimization_mode')
  })

  it('persists advanced cache enhancements for OpenAI API Key text pools', async () => {
    const account = buildAccount()
    account.credentials = {
      ...account.credentials,
      pool_mode: true,
      prompt_cache_boost_enabled: true,
      prompt_cache_boost_level: 'aggressive',
      prompt_cache_boost_upstream_hit_priority_enabled: true,
      prompt_cache_smart_routing_enabled: true,
      prompt_cache_account_relay_enabled: true,
      prompt_cache_key_optimization_enabled: true,
      prompt_cache_long_context_enhancement_enabled: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    expect(wrapper.text()).toContain('admin.accounts.promptCacheAdvancedFeatures')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    const credentials = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(credentials?.pool_mode).toBe(true)
    expect(credentials?.prompt_cache_smart_routing_enabled).toBe(true)
    expect(credentials?.prompt_cache_account_relay_enabled).toBe(true)
    expect(credentials?.prompt_cache_key_optimization_enabled).toBe(true)
    expect(credentials?.prompt_cache_long_context_enhancement_enabled).toBe(true)
  })

  it('keeps advanced cache enhancements absent when all four switches are off', async () => {
    const oauth = buildOpenAIOAuthAccount()
    for (const key of [
      'prompt_cache_smart_routing_enabled',
      'prompt_cache_account_relay_enabled',
      'prompt_cache_key_optimization_enabled',
      'prompt_cache_long_context_enhancement_enabled'
    ]) {
      delete oauth.credentials[key]
    }
    const apiKey = buildAccount()
    apiKey.credentials = {
      ...apiKey.credentials,
      pool_mode: true,
      prompt_cache_boost_enabled: true,
      prompt_cache_boost_level: 'aggressive',
      prompt_cache_boost_upstream_hit_priority_enabled: true
    }

    for (const account of [oauth, apiKey]) {
      updateAccountMock.mockReset()
      checkMixedChannelRiskMock.mockReset()
      checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
      updateAccountMock.mockResolvedValue(account)
      const wrapper = mountModal(account)

      await wrapper.get('form#edit-account-form').trigger('submit.prevent')

      const credentials = updateAccountMock.mock.calls[0]?.[1]?.credentials
      expect(credentials).not.toHaveProperty('prompt_cache_smart_routing_enabled')
      expect(credentials).not.toHaveProperty('prompt_cache_account_relay_enabled')
      expect(credentials).not.toHaveProperty('prompt_cache_key_optimization_enabled')
      expect(credentials).not.toHaveProperty('prompt_cache_long_context_enhancement_enabled')
      expect(credentials?.prompt_cache_boost_upstream_hit_priority_enabled).toBe(true)
      wrapper.unmount()
    }
  })

  it('submits independent Anthropic cache boost and isolation settings', async () => {
    const account = buildAnthropicOAuthAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.text()).toContain('admin.accounts.anthropicCacheBoost')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheBoostUpstreamHitPriority')
    expect(wrapper.text()).toContain('admin.accounts.anthropicUpstreamStrongIsolation')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    const credentials = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(credentials?.anthropic_cache_boost_enabled).toBe(true)
    expect(credentials?.anthropic_cache_boost_level).toBe('aggressive')
    expect(credentials?.anthropic_cache_boost_upstream_hit_priority_enabled).toBe(true)
    expect(credentials?.anthropic_upstream_strong_isolation_enabled).toBe(true)
    expect(credentials).not.toHaveProperty('prompt_cache_boost_enabled')
    expect(credentials).not.toHaveProperty('prompt_cache_boost_level')
    expect(credentials).not.toHaveProperty('prompt_cache_boost_upstream_hit_priority_enabled')
    expect(credentials).not.toHaveProperty('upstream_strong_isolation_enabled')
  })

  it('does not expose Anthropic API key cache boost and isolation without pool mode', async () => {
    const account = buildAnthropicAPIKeyAccount(false)
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.text()).not.toContain('admin.accounts.anthropicCacheBoost')
    expect(wrapper.text()).not.toContain('admin.accounts.anthropicUpstreamStrongIsolation')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    const credentials = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(credentials?.pool_mode).toBeUndefined()
    expect(credentials).not.toHaveProperty('anthropic_cache_boost_enabled')
    expect(credentials).not.toHaveProperty('anthropic_cache_boost_level')
    expect(credentials).not.toHaveProperty('anthropic_cache_boost_upstream_hit_priority_enabled')
    expect(credentials).not.toHaveProperty('anthropic_upstream_strong_isolation_enabled')
    expect(credentials).not.toHaveProperty('prompt_cache_boost_enabled')
    expect(credentials).not.toHaveProperty('prompt_cache_boost_upstream_hit_priority_enabled')
    expect(credentials).not.toHaveProperty('upstream_strong_isolation_enabled')
  })

  it('shows and submits Anthropic API Key upstream auth scheme from the base API Key section', async () => {
    const account = buildAnthropicAPIKeyAccount(false)
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    const field = wrapper.get('[data-testid="anthropic-apikey-auth-scheme-field"]')
    expect(field.text()).toContain('admin.accounts.anthropic.apiKeyAuthScheme')

    await wrapper.get('[data-testid="anthropic-apikey-auth-scheme-select"]').setValue('authorization_bearer')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.anthropic_apikey_auth_scheme).toBe(
      'authorization_bearer'
    )
  })

  it('clears Anthropic API Key upstream auth scheme when reset to x-api-key', async () => {
    const account = buildAnthropicAPIKeyAccount(false)
    account.extra = {
      anthropic_passthrough: true,
      anthropic_apikey_auth_scheme: 'authorization_bearer'
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="anthropic-apikey-auth-scheme-select"]').setValue('x_api_key')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    const extra = updateAccountMock.mock.calls[0]?.[1]?.extra
    expect(extra?.anthropic_passthrough).toBe(true)
    expect(extra).not.toHaveProperty('anthropic_apikey_auth_scheme')
  })

  it('keeps Anthropic API key cache boost and isolation with pool mode', async () => {
    const account = buildAnthropicAPIKeyAccount(true)
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.text()).toContain('admin.accounts.anthropicCacheBoost')
    expect(wrapper.text()).toContain('admin.accounts.promptCacheBoostUpstreamHitPriority')
    expect(wrapper.text()).toContain('admin.accounts.anthropicUpstreamStrongIsolation')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    const credentials = updateAccountMock.mock.calls[0]?.[1]?.credentials
    expect(credentials?.pool_mode).toBe(true)
    expect(credentials?.anthropic_cache_boost_enabled).toBe(true)
    expect(credentials?.anthropic_cache_boost_level).toBe('aggressive')
    expect(credentials?.anthropic_cache_boost_upstream_hit_priority_enabled).toBe(true)
    expect(credentials?.anthropic_upstream_strong_isolation_enabled).toBe(true)
    expect(credentials).not.toHaveProperty('prompt_cache_boost_enabled')
    expect(credentials).not.toHaveProperty('prompt_cache_boost_upstream_hit_priority_enabled')
    expect(credentials).not.toHaveProperty('upstream_strong_isolation_enabled')
  })

  it('reopening the same account rehydrates the OpenAI whitelist from props', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.get('[data-testid="model-whitelist-value"]').text()).toBe('gpt-5.2')

    await wrapper.get('[data-testid="rewrite-to-snapshot"]').trigger('click')
    expect(wrapper.get('[data-testid="model-whitelist-value"]').text()).toBe('gpt-5.2-2025-12-11')

    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })

    expect(wrapper.get('[data-testid="model-whitelist-value"]').text()).toBe('gpt-5.2')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.model_mapping).toEqual({
      'gpt-5.2': 'gpt-5.2'
    })
  })

  it('preserves model mappings when editing the whitelist', async () => {
    const account = buildAccount()
    account.credentials.model_mapping = {
      'gpt-5.2': 'gpt-5.2',
      'gpt-latest': 'gpt-5.2'
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.get('[data-testid="model-whitelist-value"]').text()).toBe('gpt-5.2')

    await wrapper.get('[data-testid="rewrite-to-snapshot"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.model_mapping).toEqual({
      'gpt-5.2-2025-12-11': 'gpt-5.2-2025-12-11',
      'gpt-latest': 'gpt-5.2'
    })
  })

  it('submits OpenAI compact mode and compact-only model mapping', async () => {
    const account = buildAccount()
    account.extra = {
      openai_compact_mode: 'force_on'
    }
    account.credentials = {
      ...account.credentials,
      compact_model_mapping: {
        'gpt-5.4': 'gpt-5.4-openai-compact'
      }
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_compact_mode).toBe('force_on')
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.compact_model_mapping).toEqual({
      'gpt-5.4': 'gpt-5.4-openai-compact'
    })
  })

  it('submits OpenAI APIKey Responses support override mode', async () => {
    const account = buildAccount()
    account.extra = {
      openai_responses_mode: 'force_chat_completions',
      openai_responses_supported: false
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="openai-responses-mode-select"]').setValue('force_responses')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_responses_mode).toBe('force_responses')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_responses_supported).toBe(false)
  })

  it('clears OpenAI APIKey Responses override when set back to auto', async () => {
    const account = buildAccount()
    account.extra = {
      openai_responses_mode: 'force_chat_completions',
      openai_responses_supported: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="openai-responses-mode-select"]').setValue('auto')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('openai_responses_mode')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_responses_supported).toBe(true)
  })

  it('submits OpenAI APIKey endpoint capabilities from credentials', async () => {
    const account = buildAccount()
    account.credentials.openai_capabilities = ['chat_completions']
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    expect(wrapper.findAll('input[type="checkbox"]').some((input) => (input.element as HTMLInputElement).checked)).toBe(true)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.openai_capabilities).toEqual([
      'chat_completions'
    ])
  })

  it('persists race max elapsed only while upstream concurrency race is enabled', async () => {
    const account = buildAccount()
    account.credentials = {
      ...account.credentials,
      pool_mode: true,
      pool_mode_retry_count: 20,
      upstream_concurrency_race_enabled: true,
      upstream_concurrency_race_retry_delay_ms: 20
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.upstream_concurrency_race_max_elapsed_ms).toBe(2000)

    updateAccountMock.mockClear()
    await wrapper.get('[data-testid="upstream-concurrency-race-toggle"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).not.toHaveProperty(
      'upstream_concurrency_race_max_elapsed_ms'
    )
  })

	it('submits OpenAI quota auto-pause thresholds in extra', async () => {
	  const account = buildAccount()
	  account.extra = {
		auto_pause_5h_threshold: 0.9,
		auto_pause_7d_threshold: 0.8
	  }
	  updateAccountMock.mockReset()
	  checkMixedChannelRiskMock.mockReset()
	  checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
	  updateAccountMock.mockResolvedValue(account)

	  const wrapper = mountModal(account)

	  await wrapper.get('[data-testid="auto-pause-5h-threshold"]').setValue('95')
	  await wrapper.get('[data-testid="auto-pause-7d-threshold"]').setValue('96')
	  await wrapper.get('form#edit-account-form').trigger('submit.prevent')

	  expect(updateAccountMock).toHaveBeenCalledTimes(1)
	  expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.auto_pause_5h_threshold).toBe(0.95)
	  expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.auto_pause_7d_threshold).toBe(0.96)
	})

	it('submits OpenAI quota auto-pause disable flag in extra', async () => {
	  // Toggling the per-account disable flag must persist as auto_pause_5h_disabled
	  // so an admin can exempt one account from auto-pause even when a global default
	  // threshold is configured (otherwise leaving the threshold blank would silently
	  // fall back to the global default).
	  const account = buildAccount()
	  updateAccountMock.mockReset()
	  checkMixedChannelRiskMock.mockReset()
	  checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
	  updateAccountMock.mockResolvedValue(account)

	  const wrapper = mountModal(account)

	  await wrapper.get('[data-testid="auto-pause-5h-disabled"]').trigger('click')
	  await wrapper.get('form#edit-account-form').trigger('submit.prevent')

	  expect(updateAccountMock).toHaveBeenCalledTimes(1)
	  expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.auto_pause_5h_disabled).toBe(true)
	  expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.auto_pause_7d_disabled).toBeUndefined()
	})

  it('keeps at least one OpenAI APIKey endpoint capability selected', async () => {
    const account = buildAccount()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    const chatCheckbox = wrapper.get<HTMLInputElement>(
      '[data-testid="openai-endpoint-capability-chat_completions"]'
    )
    const embeddingsCheckbox = wrapper.get<HTMLInputElement>(
      '[data-testid="openai-endpoint-capability-embeddings"]'
    )

    expect(chatCheckbox.element.checked).toBe(true)
    expect(embeddingsCheckbox.element.checked).toBe(true)

    await embeddingsCheckbox.setValue(false)

    expect(chatCheckbox.element.checked).toBe(true)
    expect(embeddingsCheckbox.element.checked).toBe(false)

    await chatCheckbox.setValue(false)

    expect(chatCheckbox.element.checked).toBe(true)
    expect(embeddingsCheckbox.element.checked).toBe(false)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.openai_capabilities).toEqual([
      'chat_completions'
    ])
  })

  it('disables text generation protocol when only embeddings requests are accepted', async () => {
    const account = buildAccount()
    account.credentials.openai_capabilities = ['embeddings']
    account.extra = {
      openai_responses_mode: 'force_responses',
      openai_responses_supported: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    const responsesModeSelect = wrapper.get<HTMLSelectElement>(
      '[data-testid="openai-responses-mode-select"]'
    )

    expect(responsesModeSelect.element.disabled).toBe(true)
    expect(wrapper.find('[data-testid="openai-responses-mode-not-applicable"]').exists()).toBe(true)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.openai_capabilities).toEqual([
      'embeddings'
    ])
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('openai_responses_mode')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.openai_responses_supported).toBe(true)
  })

  it('submits account-level Codex image generation bridge override', async () => {
    const account = buildAccount()
    account.extra = {
      codex_image_generation_bridge: false,
      codex_image_generation_bridge_enabled: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('button[data-testid="codex-image-tool-enabled"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.codex_image_generation_bridge).toBe(true)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('codex_image_generation_bridge_enabled')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('codex_image_generation_explicit_tool_policy')
  })

  it('submits Codex image tool block mode as strip policy', async () => {
    const account = buildAccount()
    account.extra = {
      codex_image_generation_bridge: true
    }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('button[data-testid="codex-image-tool-block"]').trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra?.codex_image_generation_explicit_tool_policy).toBe('strip')
    expect(updateAccountMock.mock.calls[0]?.[1]?.extra).not.toHaveProperty('codex_image_generation_bridge')
  })

  it('allows saving apikey account when backend redacted api_key but credentials_status reports it exists', async () => {
    // 新前端 + 新后端：响应已脱敏，credentials 里没有 api_key，credentials_status.has_api_key=true
    const account = buildAccount()
    account.credentials = {
      base_url: 'https://api.openai.com',
      model_mapping: { 'gpt-5.2': 'gpt-5.2' }
    }
    account.credentials_status = { has_api_key: true }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    // 用户未输入新 key 时，payload 不应带 api_key，由后端合并保留旧值
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).not.toHaveProperty('api_key')
  })

  it('allows saving apikey account against legacy backend without credentials_status', async () => {
    // 新前端 + 旧后端：credentials_status 缺失，但 credentials.api_key 仍是明文，应允许保存
    const account = buildAccount()
    // 显式确保没有 credentials_status
    expect(account.credentials_status).toBeUndefined()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    // 旧后端响应未脱敏，原 api_key 会随 currentCredentials 一起传回去（旧行为，等价于无操作）
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.api_key).toBe('sk-test')
  })

  it('blocks apikey save when neither credentials_status nor legacy api_key indicates existence', async () => {
    const account = buildAccount()
    account.credentials = {
      base_url: 'https://api.openai.com'
    }
    // 既没有 credentials_status 也没有旧的 api_key
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).not.toHaveBeenCalled()
  })

  it('allows saving Vertex SA account when backend redacted service_account_json but credentials_status reports it exists', async () => {
    // 新前端 + 新后端：响应已脱敏，credentials 里没有 service_account_json，credentials_status.has_service_account_json=true
    const account = buildVertexAccount()
    account.credentials = {
      project_id: 'demo-project',
      client_email: 'sa@example.iam.gserviceaccount.com',
      location: 'us-central1',
      tier_id: 'vertex'
    }
    account.credentials_status = { has_service_account_json: true }
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.project_id).toBe('demo-project')
  })

  it('allows saving Vertex SA account against legacy backend without credentials_status', async () => {
    // 新前端 + 旧后端：credentials_status 缺失，但 credentials.service_account_json 仍是明文，应允许保存
    const account = buildVertexAccount()
    expect(account.credentials_status).toBeUndefined()
    expect(account.credentials.service_account_json).toBeTruthy()
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
  })

  it('blocks Vertex SA save when neither credentials_status nor legacy json indicates existence', async () => {
    const account = buildVertexAccount()
    account.credentials = {
      project_id: 'demo-project',
      client_email: 'sa@example.iam.gserviceaccount.com',
      location: 'us-central1',
      tier_id: 'vertex'
    }
    // 既没有 credentials_status 也没有旧的 service_account_json
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')

    expect(updateAccountMock).not.toHaveBeenCalled()
  })
})
