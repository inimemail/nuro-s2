import { describe, expect, it, vi, beforeEach } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

const { createAccountMock, checkMixedChannelRiskMock } = vi.hoisted(() => ({
  createAccountMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn(),
    showWarning: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    isSimpleMode: true
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createAccountMock,
      checkMixedChannelRisk: checkMixedChannelRiskMock,
      generateAuthUrl: vi.fn(),
      exchangeCode: vi.fn(),
      importCodexSession: vi.fn(),
      refreshOpenAIToken: vi.fn(),
      startOpenAIDeviceAuth: vi.fn(),
      exchangeOpenAIDeviceAuth: vi.fn()
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([])
    },
    gemini: {
      generateAuthUrl: vi.fn(),
      exchangeCode: vi.fn(),
      getCapabilities: vi.fn().mockResolvedValue({ ai_studio_oauth_enabled: false })
    },
    antigravity: {
      generateAuthUrl: vi.fn(),
      exchangeCode: vi.fn(),
      refreshAntigravityToken: vi.fn()
    },
    grok: {
      generateAuthUrl: vi.fn(),
      exchangeCode: vi.fn(),
      refreshGrokToken: vi.fn()
    }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([])
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

import CreateAccountModal from '../CreateAccountModal.vue'

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
  template: '<div data-testid="model-whitelist-stub" />'
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

function mountModal() {
  return mount(CreateAccountModal, {
    props: {
      show: true,
      proxies: [],
      groups: []
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        Select: SelectStub,
        Icon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        GroupSelector: true,
        OAuthAuthorizationFlow: true,
        ModelWhitelistSelector: ModelWhitelistSelectorStub,
        QuotaLimitCard: true
      }
    }
  })
}

describe('CreateAccountModal', () => {
  beforeEach(() => {
    createAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    createAccountMock.mockResolvedValue({})
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
  })

  it('shows and submits Anthropic API Key upstream auth scheme from the base API Key section', async () => {
    const wrapper = mountModal()

    await wrapper.findAll('button').find((button) => button.text().includes('admin.accounts.claudeConsole'))!.trigger('click')
    await flushPromises()

    const field = wrapper.get('[data-testid="anthropic-apikey-auth-scheme-field"]')
    expect(field.text()).toContain('admin.accounts.anthropic.apiKeyAuthScheme')

    await wrapper.get('input[data-tour="account-form-name"]').setValue('Anthropic Bearer Key')
    await wrapper.get('input[placeholder="sk-ant-..."]').setValue('sk-ant-test')
    await wrapper.get('[data-testid="anthropic-apikey-auth-scheme-select"]').setValue('authorization_bearer')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]).toMatchObject({
      name: 'Anthropic Bearer Key',
      platform: 'anthropic',
      type: 'apikey',
      credentials: {
        base_url: 'https://api.anthropic.com',
        api_key: 'sk-ant-test'
      },
      extra: {
        anthropic_kiro: false,
        anthropic_apikey_auth_scheme: 'authorization_bearer'
      }
    })
  })

  it('does not submit Anthropic API Key upstream auth scheme when x-api-key is selected', async () => {
    const wrapper = mountModal()

    await wrapper.findAll('button').find((button) => button.text().includes('admin.accounts.claudeConsole'))!.trigger('click')
    await flushPromises()

    await wrapper.get('input[data-tour="account-form-name"]').setValue('Anthropic Default Key')
    await wrapper.get('input[placeholder="sk-ant-..."]').setValue('sk-ant-test')
    await wrapper.get('[data-testid="anthropic-apikey-auth-scheme-select"]').setValue('x_api_key')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra).not.toHaveProperty('anthropic_apikey_auth_scheme')
  })

  it('submits cache creation optimization for a non-pool OpenAI API Key account', async () => {
    const wrapper = mountModal()

    await wrapper.findAll('button').find((button) => button.text().trim() === 'OpenAI')!.trigger('click')
    await wrapper.findAll('button').find((button) => button.text().includes('API Key'))!.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.promptCacheCreationOptimization')
    await wrapper.get('input[data-tour="account-form-name"]').setValue('OpenAI Key')
    await wrapper.get('input[placeholder="sk-proj-..."]').setValue('sk-test')
    await wrapper.get('[data-testid="prompt-cache-creation-optimization-toggle"]').trigger('click')
    await wrapper.get('[data-testid="prompt-cache-creation-mode-suppress"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]).toMatchObject({
      platform: 'openai',
      type: 'apikey',
      credentials: {
        openai_prompt_cache_creation_optimization_enabled: true,
        openai_prompt_cache_creation_optimization_mode: 'suppress'
      }
    })
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).not.toHaveProperty('pool_mode')
  })

  it('does not write cache creation optimization credentials while the switch stays off', async () => {
    const wrapper = mountModal()

    await wrapper.findAll('button').find((button) => button.text().trim() === 'OpenAI')!.trigger('click')
    await wrapper.findAll('button').find((button) => button.text().includes('API Key'))!.trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="prompt-cache-creation-optimization-toggle"]').attributes('aria-pressed')).toBe('false')
    await wrapper.get('input[data-tour="account-form-name"]').setValue('OpenAI Default Key')
    await wrapper.get('input[placeholder="sk-proj-..."]').setValue('sk-test')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const credentials = createAccountMock.mock.calls[0]?.[0]?.credentials
    expect(credentials).not.toHaveProperty('openai_prompt_cache_creation_optimization_enabled')
    expect(credentials).not.toHaveProperty('openai_prompt_cache_creation_optimization_mode')
  })

  it('resets API key timeout placeholder stages after disabling and enabling again', async () => {
    const wrapper = mountModal()

    await wrapper.findAll('button').find((button) => button.text().trim() === 'OpenAI')!.trigger('click')
    await wrapper.findAll('button').find((button) => button.text().includes('API Key'))!.trigger('click')
    await flushPromises()

    const toggle = wrapper.get('[data-testid="apikey-first-token-timeout-placeholder-toggle"]')
    await toggle.trigger('click')
    await wrapper.get('[data-testid="stage-1-placeholder"]').setValue(1200)
    await wrapper.get('[data-testid="stage-1-guard"]').setValue(3600)
    await wrapper.get('[data-testid="add-first-token-stage"]').trigger('click')
    expect(wrapper.find('[data-testid="stage-2-placeholder"]').exists()).toBe(true)

    await toggle.trigger('click')
    await toggle.trigger('click')

    expect((wrapper.get('[data-testid="stage-1-placeholder"]').element as HTMLInputElement).value).toBe('800')
    expect((wrapper.get('[data-testid="stage-1-guard"]').element as HTMLInputElement).value).toBe('5000')
    expect((wrapper.get('[data-testid="stage-2-placeholder"]').element as HTMLInputElement).value).toBe('3000')
    expect((wrapper.get('[data-testid="stage-2-guard"]').element as HTMLInputElement).value).toBe('10000')
    expect((wrapper.get('[data-testid="stage-3-placeholder"]').element as HTMLInputElement).value).toBe('5000')
    expect((wrapper.get('[data-testid="stage-3-guard"]').element as HTMLInputElement).value).toBe('15000')
    expect((wrapper.get('[data-testid="stage-4-placeholder"]').element as HTMLInputElement).value).toBe('10000')
    expect((wrapper.get('[data-testid="stage-4-guard"]').element as HTMLInputElement).value).toBe('30000')
  })

  it('submits the API key placeholder maximum with an uncapped guard', async () => {
    const wrapper = mountModal()

    await wrapper.findAll('button').find((button) => button.text().trim() === 'OpenAI')!.trigger('click')
    await wrapper.findAll('button').find((button) => button.text().includes('API Key'))!.trigger('click')
    await flushPromises()

    await wrapper.get('input[data-tour="account-form-name"]').setValue('OpenAI High Timeout Key')
    await wrapper.get('input[placeholder="sk-proj-..."]').setValue('sk-test')
    await wrapper.get('[data-testid="apikey-first-token-timeout-placeholder-toggle"]').trigger('click')
    await wrapper.get('[data-testid="stage-4-placeholder"]').setValue(100000)
    await wrapper.get('[data-testid="stage-4-guard"]').setValue(900000)
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra).toMatchObject({
      openai_apikey_first_token_timeout_placeholder_ms: 800,
      openai_apikey_first_token_timeout_placeholder_guard_max_ms: 5000,
      openai_apikey_first_token_timeout_placeholder_stages: [
        { stage: 1, placeholder_ms: 800, guard_max_ms: 5000 },
        { stage: 2, placeholder_ms: 3000, guard_max_ms: 10000 },
        { stage: 3, placeholder_ms: 5000, guard_max_ms: 15000 },
        { stage: 4, placeholder_ms: 100000, guard_max_ms: 900000 }
      ]
    })
  })

  it('submits explicit OpenAI pool retry conditions without changing retry timing fields', async () => {
    const wrapper = mountModal()

    await wrapper.findAll('button').find((button) => button.text().trim() === 'OpenAI')!.trigger('click')
    await wrapper.findAll('button').find((button) => button.text().includes('API Key'))!.trigger('click')
    await flushPromises()

    await wrapper.get('input[data-tour="account-form-name"]').setValue('OpenAI Pool Key')
    await wrapper.get('input[placeholder="sk-proj-..."]').setValue('sk-test')
    await wrapper.get('[data-testid="pool-mode-toggle"]').trigger('click')
    await wrapper.get('[data-testid="pool-retry-remove-status-code-401"]').trigger('click')
    await wrapper.get('[data-testid="pool-mode-builtin-retry-toggle"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      pool_mode: true,
      pool_mode_retry_status_codes: [403, 429],
      pool_mode_builtin_retry_enabled: false
    })
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).not.toHaveProperty(
      'upstream_concurrency_race_retry_delay_ms'
    )
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).not.toHaveProperty(
      'upstream_concurrency_race_max_elapsed_ms'
    )
  })

  it('shows Kimi billing/protocol choices and persists the selected coding preset', async () => {
    const wrapper = mountModal()
    await wrapper.findAll('button').find((button) => button.text().trim() === 'Kimi')!.trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="cn-billing-coding-plan"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="cn-protocol-chat_completions"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="cn-protocol-anthropic"]').exists()).toBe(true)
    const baseUrl = wrapper.get('input[placeholder="https://api.moonshot.cn/v1"]')
    await baseUrl.setValue('https://gateway.example.test/kimi')
    await wrapper.get('[data-testid="cn-billing-coding-plan"]').trigger('click')
    expect((baseUrl.element as HTMLInputElement).value).toBe('https://gateway.example.test/kimi')
    await wrapper.get('[data-testid="cn-base-url-coding"]').trigger('click')
    await wrapper.get('input[data-tour="account-form-name"]').setValue('Kimi Coding')
    await wrapper.get('input[placeholder="sk-..."]').setValue('sk-kimi-test')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock.mock.calls[0]?.[0]).toMatchObject({
      platform: 'kimi',
      type: 'apikey',
      credentials: {
        base_url: 'https://api.kimi.com/coding/v1',
        api_key: 'sk-kimi-test',
        api_base_urls: {
          chat_completions: 'https://api.kimi.com/coding/v1',
          anthropic: 'https://api.kimi.com/coding'
        }
      },
      extra: { cn_billing_mode: 'coding_plan', cn_api_mode: 'adaptive' }
    })
  })

  it('keeps DeepSeek pay-as-you-go only and exposes Responses protocol', async () => {
    const wrapper = mountModal()
    await wrapper.findAll('button').find((button) => button.text().trim() === 'DeepSeek')!.trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="cn-billing-coding-plan"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="cn-protocol-responses"]').exists()).toBe(true)
    await wrapper.get('[data-testid="cn-protocol-responses"]').trigger('click')
    expect(wrapper.find('[data-testid="cn-base-url-deepseek"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="cn-base-url-deepseekResponses"]').exists()).toBe(true)
    await wrapper.get('input[data-tour="account-form-name"]').setValue('DeepSeek Responses')
    await wrapper.get('input[placeholder="sk-..."]').setValue('sk-deepseek-test')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock.mock.calls[0]?.[0]).toMatchObject({
      credentials: { base_url: 'https://api.deepseek.com', api_key: 'sk-deepseek-test' },
      extra: {
        cn_billing_mode: 'payg',
        cn_api_mode: 'responses'
      }
    })
  })
})
