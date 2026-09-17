import { describe, expect, it } from 'vitest'
import { resolveOpenAICompactState } from '../openaiCompact'

describe('Compact protocol state', () => {
  it('keeps native and standalone results independent', () => {
    const extra = { openai_compact_supported: true, openai_native_compact_supported: false }
    expect(resolveOpenAICompactState(extra)).toBe('active')
    expect(resolveOpenAICompactState(extra, true)).toBe('blocked')
    expect(resolveOpenAICompactState({ openai_compact_supported: false }, true)).toBe('auto')
  })
  it('does not perpetuate a historical native false negative for standalone', () => {
    const extra = { openai_compact_supported: false, openai_compact_last_error: 'native remote compaction v2 unsupported' }
    expect(resolveOpenAICompactState(extra)).toBe('auto')
    expect(resolveOpenAICompactState({ ...extra, openai_compact_mode: 'force_off' })).toBe('blocked')
  })
  it.each([false, true])('preserves manual overrides for native=%s', native => {
    expect(resolveOpenAICompactState({ openai_compact_mode: 'force_on', openai_native_compact_supported: false }, native)).toBe('active')
    expect(resolveOpenAICompactState({ openai_compact_mode: 'force_off', openai_compact_supported: true }, native)).toBe('blocked')
  })
})
