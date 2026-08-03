import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

import {
  buildUpstreamBillingGuardLimitPayload,
  isUpstreamBillingGuardPlatform,
  normalizeUpstreamBillingGuardLimit
} from '../groupsUpstreamBillingGuard'

const currentDir = dirname(fileURLToPath(import.meta.url))
const groupsViewSource = readFileSync(resolve(currentDir, '../GroupsView.vue'), 'utf8')

describe('groups upstream billing guard', () => {
  it('keeps zero and finite OpenAI thresholds', () => {
    expect(buildUpstreamBillingGuardLimitPayload('openai', 0)).toBe(0)
    expect(buildUpstreamBillingGuardLimitPayload('openai', '0.065')).toBe(0.065)
    expect(buildUpstreamBillingGuardLimitPayload('openai', '1.5')).toBe(1.5)
  })

  it('serializes blank values for every supported platform', () => {
    expect(buildUpstreamBillingGuardLimitPayload('openai', '')).toBeNull()
    expect(buildUpstreamBillingGuardLimitPayload('anthropic', 2)).toBe(2)
  })

  it('drops a stale hidden limit after switching to an unsupported platform', () => {
    expect(buildUpstreamBillingGuardLimitPayload('other', 0.065)).toBeNull()
  })

  it('rejects invalid configured values', () => {
    expect(normalizeUpstreamBillingGuardLimit(-1)).toBeUndefined()
    expect(normalizeUpstreamBillingGuardLimit('not-a-number')).toBeUndefined()
  })

  it('renders the field for all API-key billing probe platforms', () => {
    for (const platform of ['openai', 'anthropic', 'gemini', 'grok', 'antigravity'] as const) {
      expect(isUpstreamBillingGuardPlatform(platform)).toBe(true)
    }
    expect(groupsViewSource).toContain('v-if="isUpstreamBillingGuardPlatform(createForm.platform)"')
    expect(groupsViewSource).toContain('v-if="isUpstreamBillingGuardPlatform(editForm.platform)"')
    expect(groupsViewSource).toContain('data-testid="create-group-upstream-billing-guard-limit"')
    expect(groupsViewSource).toContain('data-testid="edit-group-upstream-billing-guard-limit"')
    expect(groupsViewSource).toMatch(/step="0\.001"[\s\S]*data-testid="create-group-upstream-billing-guard-limit"/)
    expect(groupsViewSource).toMatch(/step="0\.001"[\s\S]*data-testid="edit-group-upstream-billing-guard-limit"/)
  })
})
