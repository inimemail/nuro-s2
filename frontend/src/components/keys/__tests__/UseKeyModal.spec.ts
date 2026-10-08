import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard: vi.fn().mockResolvedValue(true)
  })
}))

import UseKeyModal from '../UseKeyModal.vue'

describe('UseKeyModal', () => {
  const mountOpenAI = (baseUrl = 'https://example.com/v1', apiKey = 'sk-test') => mount(UseKeyModal, {
    props: { show: true, apiKey, baseUrl, platform: 'openai', groupId: 1 },
    global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }, Icon: true } }
  })

  it('keeps the ordinary default config unchanged and escapes auth JSON', () => {
    const wrapper = mountOpenAI()
    expect(wrapper.findAll('pre code')[0].text()).toBe(`model_provider = "OpenAI"
model = "gpt-5.5"
review_model = "gpt-5.5"
model_reasoning_effort = "xhigh"
disable_response_storage = true
network_access = "enabled"
windows_wsl_setup_acknowledged = true

[model_providers.OpenAI]
name = "OpenAI"
base_url = "https://example.com/v1"
wire_api = "responses"
requires_openai_auth = true

[features]
goals = true`)
    expect(wrapper.findAll('pre code')[1].text()).toBe('{\n  "OPENAI_API_KEY": "sk-test"\n}')
    const hostile = 'sk-"\\\nsecret'
    const escaped = mountOpenAI('https://example.com/"\\\npath', hostile)
    expect(JSON.parse(escaped.findAll('pre code')[1].text())).toEqual({ OPENAI_API_KEY: hostile })
    expect(escaped.findAll('pre code')[0].text()).toContain('base_url = "https://example.com/\\"\\\\\\npath/v1"')
  })

  it.each([
    ['https://example.com', 'https://example.com/backend-api/codex/models'],
    ['https://example.com/v1/', 'https://example.com/backend-api/codex/models'],
    ['https://example.com/prefix/v1?key=secret#hash', 'https://example.com/prefix/backend-api/codex/models'],
    ['https://example.com/prefix/backend-api/codex/', 'https://example.com/prefix/backend-api/codex/models']
  ])('builds a gateway catalog URL from %s without client secrets', async (base, expected) => {
    const wrapper = mountOpenAI(base)
    await wrapper.get('[data-catalog-source="remote"]').trigger('click')
    expect(wrapper.findAll('pre code')[0].text()).toContain(`model_catalog_url = "${expected}"`)
    expect(wrapper.findAll('pre code')[0].text()).toContain('api_key_model_discovery = true')
    expect(wrapper.findAll('pre code')[0].text()).toContain('requires_openai_auth = true')
    const ws = wrapper.findAll('button').find(button => button.text().includes('cliTabs.codexCliWs'))!
    await ws.trigger('click')
    expect(wrapper.findAll('pre code')[0].text()).toContain('supports_websockets = true')
    expect(wrapper.findAll('pre code')[0].text()).toContain(`model_catalog_url = "${expected}"`)
  })

  it('resets catalog mode for key, group and dialog sessions', async () => {
    const wrapper = mountOpenAI()
    for (const props of [{ apiKey: 'sk-new' }, { groupId: 2 }, { show: false }, { show: true }, { baseUrl: 'https://other.example/v1' }]) {
      await wrapper.get('[data-catalog-source="remote"]').trigger('click')
      await wrapper.setProps(props)
      expect(wrapper.get('[data-catalog-source="default"]').attributes('aria-pressed')).toBe('true')
      expect(wrapper.findAll('pre code')[0].text()).not.toContain('model_catalog_url')
    }
    await wrapper.setProps({ platform: 'grok' })
    expect(wrapper.find('[data-catalog-source]').exists()).toBe(false)
  })

  it.each(['invalid', 'javascript:alert(1)', 'https://user:pass@example.com'])('disables remote catalog for invalid public base %s', base => {
    const wrapper = mountOpenAI(base)
    expect(wrapper.get('[data-catalog-source="remote"]').attributes('disabled')).toBeDefined()
  })

  it('renders Grok CLI and OpenCode Responses setup for Grok groups', async () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'xai-test-key',
        baseUrl: 'https://example.com/v1',
        platform: 'grok'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const grokConfig = wrapper.findAll('pre code')
      .map((code) => code.text())
      .find((content) => content.includes('[model."sub2api-grok"]'))
    expect(grokConfig).toBeDefined()
    expect(grokConfig).toContain('model = "grok-4.5"')
    expect(grokConfig).toContain('base_url = "https://example.com/v1"')
    expect(grokConfig).toContain('api_key = "xai-test-key"')
    expect(grokConfig).toContain('api_backend = "responses"')

    const opencodeTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.opencode')
    )
    expect(opencodeTab).toBeDefined()
    await opencodeTab!.trigger('click')
    await nextTick()

    const parsed = JSON.parse(wrapper.find('pre code').text())
    expect(parsed.provider.grok.npm).toBe('@ai-sdk/openai')
    expect(parsed.provider.grok.options).toEqual({
      baseURL: 'https://example.com/v1',
      apiKey: 'xai-test-key'
    })
    expect(parsed.provider.grok.models['grok-4.5']).toBeDefined()
    expect(parsed.provider.grok.models['grok-composer-2.5-fast']).toBeDefined()
  })

  it('renders GPT-5.5 and goals feature in OpenAI Codex config', () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-test',
        baseUrl: 'https://example.com/v1',
        platform: 'openai'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const codeBlocks = wrapper.findAll('pre code').map((code) => code.text())
    const configToml = codeBlocks.find((content) => content.includes('model_provider = "OpenAI"'))

    expect(configToml).toBeDefined()
    expect(configToml).toContain('model = "gpt-5.5"')
    expect(configToml).toContain('review_model = "gpt-5.5"')
    expect(configToml).not.toContain('model = "gpt-5.4"')
    expect(configToml).not.toContain('model_context_window')
    expect(configToml).not.toContain('model_auto_compact_token_limit')
    expect(configToml).toContain('[features]\ngoals = true')
  })

  it('renders GPT-5.5 and goals feature in OpenAI Codex WebSocket config', async () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-test',
        baseUrl: 'https://example.com/v1',
        platform: 'openai'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const wsTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.codexCliWs')
    )

    expect(wsTab).toBeDefined()
    await wsTab!.trigger('click')
    await nextTick()

    const codeBlocks = wrapper.findAll('pre code').map((code) => code.text())
    const configToml = codeBlocks.find((content) => content.includes('supports_websockets = true'))

    expect(configToml).toBeDefined()
    expect(configToml).toContain('model = "gpt-5.5"')
    expect(configToml).toContain('review_model = "gpt-5.5"')
    expect(configToml).not.toContain('model = "gpt-5.4"')
    expect(configToml).not.toContain('model_context_window')
    expect(configToml).not.toContain('model_auto_compact_token_limit')
    expect(configToml).toContain('[features]\nresponses_websockets_v2 = true\ngoals = true')
  })

  it('renders GPT-5.4 mini entry in OpenCode config', async () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-test',
        baseUrl: 'https://example.com/v1',
        platform: 'openai'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const opencodeTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.opencode')
    )

    expect(opencodeTab).toBeDefined()
    await opencodeTab!.trigger('click')
    await nextTick()

    const codeBlock = wrapper.find('pre code')
    expect(codeBlock.exists()).toBe(true)
    expect(codeBlock.text()).toContain('"name": "GPT-5.4 Mini"')
    expect(codeBlock.text()).not.toContain('"name": "GPT-5.4 Nano"')
  })
})
