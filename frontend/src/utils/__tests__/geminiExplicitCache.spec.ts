import { beforeEach, describe, expect, it } from 'vitest'
import { cacheCreateBody, cacheMoney, cacheOperationID, cacheTimeLeft } from '../geminiExplicitCache'
import { savePendingCacheOperation, readPendingCacheOperation, clearPendingCacheOperation, pendingCacheStorageKey } from '../geminiCachePending'

describe('Gemini cache quotation and operation safety', () => {
  beforeEach(() => localStorage.clear())
  it('keeps exact text and validates the TTL before quotation', () => {
    expect(cacheCreateBody('models/gemini-2.5-flash', '  context\n', 1, ' demo ')).toEqual({ model: 'models/gemini-2.5-flash', contents: [{ role: 'user', parts: [{ text: '  context\n' }] }], displayName: 'demo', ttl: '3600s' })
    for (const hours of [NaN, Infinity, 0, 25]) expect(cacheCreateBody('gemini-2.5-flash', 'x', hours, '')).toBeNull()
    expect(cacheCreateBody('gemini-2.5-flash', ' ', 1, '')).toBeNull()
    expect(cacheCreateBody('gemini-2.5-flash/evil', 'x', 1, '')).toBeNull()
  })
  it('never renders malformed amounts or expiry as a valid result', () => {
    expect(cacheMoney(undefined)).toBe('—')
    expect(cacheMoney('NaN')).toBe('—')
    expect(cacheTimeLeft('invalid')).toBe('—')
    expect(cacheTimeLeft('2026-10-13T01:01:01Z', Date.parse('2026-10-13T00:00:00Z'))).toBe('1:01:01')
  })
  it('persists only the owner-specific operation metadata and clears the matching operation', () => {
    const id = cacheOperationID()
    expect(id).toMatch(/^[a-f0-9]{32}$/)
    const pending = { keyID: 7, id, kind: 'create' as const, createdAt: Date.now() }
    savePendingCacheOperation(3, pending)
    expect(readPendingCacheOperation(3)).toEqual(pending)
    expect(readPendingCacheOperation(4)).toBeNull()
    clearPendingCacheOperation(3, 'different')
    expect(readPendingCacheOperation(3)).toEqual(pending)
    clearPendingCacheOperation(3, id)
    expect(readPendingCacheOperation(3)).toBeNull()
    localStorage.setItem(pendingCacheStorageKey(3), '{"keyID":0,"id":"bad","kind":"create"}')
    expect(readPendingCacheOperation(3)).toBeNull()
  })
})
