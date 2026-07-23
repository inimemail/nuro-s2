import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

import {
  buildUpstreamBillingGuardLimitPayload,
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

  it('serializes blank values and non-OpenAI platforms as unrestricted', () => {
    expect(buildUpstreamBillingGuardLimitPayload('openai', '')).toBeNull()
    expect(buildUpstreamBillingGuardLimitPayload('anthropic', 2)).toBeNull()
  })

  it('rejects invalid configured values', () => {
    expect(normalizeUpstreamBillingGuardLimit(-1)).toBeUndefined()
    expect(normalizeUpstreamBillingGuardLimit('not-a-number')).toBeUndefined()
  })

  it('renders the field only in OpenAI create and edit forms', () => {
    expect(groupsViewSource).toContain("v-if=\"createForm.platform === 'openai'\"")
    expect(groupsViewSource).toContain("v-if=\"editForm.platform === 'openai'\"")
    expect(groupsViewSource).toContain('data-testid="create-group-upstream-billing-guard-limit"')
    expect(groupsViewSource).toContain('data-testid="edit-group-upstream-billing-guard-limit"')
    expect(groupsViewSource).toMatch(/step="0\.001"[\s\S]*data-testid="create-group-upstream-billing-guard-limit"/)
    expect(groupsViewSource).toMatch(/step="0\.001"[\s\S]*data-testid="edit-group-upstream-billing-guard-limit"/)
  })
})
