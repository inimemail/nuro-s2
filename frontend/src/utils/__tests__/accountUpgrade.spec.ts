import { beforeEach, describe, expect, it, vi } from 'vitest'
import { loadAccountFilters, normalizeAccountFilters, saveAccountFilters } from '../accountFilters'
import { supportsAccountQuota } from '../accountQuota'

describe('account upgrade capabilities and preferences', () => {
  beforeEach(() => localStorage.clear())
  it('adds Gemini Vertex quota without enabling unrelated service accounts', () => {
    expect(supportsAccountQuota({ platform: 'gemini', type: 'service_account' })).toBe(true)
    expect(supportsAccountQuota({ platform: 'anthropic', type: 'service_account' })).toBe(false)
    expect(supportsAccountQuota({ platform: 'gemini', type: 'oauth' })).toBe(false)
    expect(supportsAccountQuota({ type: 'apikey' })).toBe(true)
    expect(supportsAccountQuota(null)).toBe(false)
  })
  it('validates values and stores only platform and status in a user scope', () => {
    saveAccountFilters('admin:1', { platform: 'gemini', status: 'active', search: 'private', group: '3' })
    expect(loadAccountFilters('admin:1')).toEqual({ platform: 'gemini', status: 'active' })
    expect(loadAccountFilters('admin:2')).toEqual({ platform: '', status: '' })
    expect(normalizeAccountFilters({ platform: 'unknown', status: ['active'] })).toEqual({ platform: '', status: '' })
    expect(localStorage.getItem('account-filters:v1:admin:1')).not.toContain('private')
  })
  it('preserves every supported account platform', () => {
    for (const platform of ['anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'typesafe']) {
      expect(normalizeAccountFilters({ platform, status: 'active' })).toEqual({ platform, status: 'active' })
    }
  })
  it('handles corrupt and unavailable storage', () => {
    localStorage.setItem('account-filters:v1:admin:1', 'broken')
    expect(loadAccountFilters('admin:1')).toEqual({ platform: '', status: '' })
    const spy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('unavailable') })
    expect(() => saveAccountFilters('admin:1', {})).not.toThrow()
    spy.mockRestore()
  })
})
