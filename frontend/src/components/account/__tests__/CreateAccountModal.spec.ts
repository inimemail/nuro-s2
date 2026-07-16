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
})
