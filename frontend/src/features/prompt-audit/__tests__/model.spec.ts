import { describe, expect, it } from 'vitest'
import { configToForm, deleteFilter, displayableUpdatedAt, formToUpdate } from '../model'
import type { PromptAuditConfig } from '../types'

function publicConfig(): PromptAuditConfig {
  return {
    enabled: true,
    mode: 'async_audit',
    worker_count: 2,
    queue_capacity: 2048,
    all_groups: false,
    group_ids: [9, 3],
    scanners: ['jailbreak'],
    endpoints: [{
      id: 'guard-a',
      name: 'Guard A',
      base_url: 'https://guard.invalid',
      model: 'guard',
      timeout_ms: 3000,
      enabled: true,
      has_token: true,
      token_status: 'configured',
      allow_private: true,
      allowed_cidrs: ['10.0.0.0/8']
    }],
    store_pass_events: false,
    retention_days: 7,
    version: 11,
    updated_at: '2026-07-17T00:00:00Z'
  }
}

describe('Prompt Audit form model', () => {
  it('hides the zero timestamp returned by an unsaved default config', () => {
    expect(displayableUpdatedAt('0001-01-01T00:00:00Z')).toBe('')
    expect(displayableUpdatedAt('not-a-time')).toBe('')
    expect(displayableUpdatedAt('2026-07-17T00:00:00Z')).toBe('2026-07-17T00:00:00Z')
  })

  it('never hydrates a saved token into editable frontend state', () => {
    const form = configToForm(publicConfig())

    expect(form.endpoints[0].has_token).toBe(true)
    expect(form.endpoints[0].token).toBe('')
    expect(form.endpoints[0].clear_token).toBe(false)
  })

  it('omits an empty token so save preserves it and canonicalizes CIDRs', () => {
    const form = configToForm(publicConfig())
    form.endpoints[0].allowed_cidrs_text = '10.0.0.0/8\n192.168.0.0/16, 10.0.0.0/8'

    const update = formToUpdate(form)

    expect(update.expected_version).toBe(11)
    expect(update.group_ids).toEqual([3, 9])
    expect(update.endpoints[0]).not.toHaveProperty('token')
    expect(update.endpoints[0].clear_token).toBe(false)
    expect(update.endpoints[0].allowed_cidrs).toEqual(['10.0.0.0/8', '192.168.0.0/16'])
  })

  it('sends clear_token only when the administrator selects it', () => {
    const form = configToForm(publicConfig())
    form.endpoints[0].clear_token = true

    expect(formToUpdate(form).endpoints[0]).toMatchObject({ clear_token: true })
  })

  it('uses only active filters for bounded deletion previews', () => {
    expect(deleteFilter({
      page: 8,
      page_size: 20,
      decision: 'critical',
      risk_level: '',
      search: '  request-1  '
    })).toEqual({
      page: 1,
      page_size: 100,
      decision: 'critical',
      search: 'request-1'
    })
  })
})
