import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import EventDetailDialog from '../components/EventDetailDialog.vue'
import { usePromptAuditCopy } from '../copy'
import { ref } from 'vue'
import type { PromptAuditEvent } from '../types'

const BaseDialogStub = {
  props: ['show', 'title'],
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
}

describe('Prompt Audit event details', () => {
  it('renders only the redacted schema and ignores unexpected full-prompt fields', () => {
    const event = {
      id: 1,
      request_id: 'req-1',
      user_id: 2,
      user_email: 'user@example.com',
      api_key_id: 3,
      api_key_name: 'key',
      group_name: 'default',
      provider: 'openai',
      endpoint: '/v1/responses',
      protocol: 'responses',
      model: 'gpt-test',
      prompt_hash: 'hash-only',
      redacted_preview: 'safe [REDACTED] preview',
      prompt_length: 24,
      message_count: 1,
      stage: 'http',
      decision: 'pass',
      risk_level: 'low',
      action: 'Allow',
      categories: [],
      scanner_backend: 'qwen3guard-openai',
      scanner_version: 'guard',
      guard_endpoint_id: 'guard-a',
      latency_ms: 12,
      created_at: '2026-07-17T00:00:00Z',
      prompt: 'FULL SECRET PROMPT MUST NOT RENDER'
    } as PromptAuditEvent & { prompt: string }
    const copy = usePromptAuditCopy(ref('en')).value

    const wrapper = mount(EventDetailDialog, {
      props: { event, copy },
      global: { stubs: { BaseDialog: BaseDialogStub, Icon: true } }
    })

    expect(wrapper.text()).toContain('safe [REDACTED] preview')
    expect(wrapper.text()).toContain('hash-only')
    expect(wrapper.text()).not.toContain('FULL SECRET PROMPT MUST NOT RENDER')
  })
})
