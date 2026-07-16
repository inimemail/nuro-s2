import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import OAuthAuthorizationFlow from '../OAuthAuthorizationFlow.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copied: false, copyToClipboard: vi.fn() }),
}))

describe('OAuthAuthorizationFlow Agent Identity import', () => {
  it('shows a dedicated mode and emits its auth.json through the Codex importer', async () => {
    const wrapper = mount(OAuthAuthorizationFlow, {
      props: {
        addMethod: 'oauth',
        platform: 'openai',
        showCookieOption: false,
        showCodexSessionImportOption: true,
        showAgentIdentityOption: true,
      },
      global: { stubs: { Icon: true } },
    })

    const agentRadio = wrapper.get('input[value="agent_identity"]')
    await agentRadio.setValue(true)

    expect(wrapper.text()).toContain('admin.accounts.oauth.openai.agentIdentityDesc')
    const input = wrapper.get('textarea')
    expect(input.attributes('placeholder')).toBe('admin.accounts.oauth.openai.agentIdentityPlaceholder')

    const content = JSON.stringify({ auth_mode: 'agentIdentity', agent_identity: { task_id: 'task-1' } })
    await input.setValue(content)
    const importButton = wrapper.findAll('button').find(button =>
      button.text().includes('admin.accounts.oauth.openai.codexSessionImportAndCreate')
    )
    expect(importButton).toBeDefined()
    await importButton!.trigger('click')

    expect(wrapper.emitted('import-codex-session')).toEqual([[content]])
    expect(wrapper.emitted('update:inputMethod')).toContainEqual(['agent_identity'])
  })
})
