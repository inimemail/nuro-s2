export type OpenAICompactState = 'active' | 'blocked' | 'auto'

export function resolveOpenAICompactState(extra: Record<string, unknown> | undefined, native = false): OpenAICompactState {
  const mode = String(extra?.openai_compact_mode ?? 'auto').trim().toLowerCase()
  if (mode === 'force_on') return 'active'
  if (mode === 'force_off') return 'blocked'
  if (!native && String(extra?.openai_compact_last_error ?? '').includes('native remote compaction v2 unsupported')) return 'auto'
  const supported = extra?.[native ? 'openai_native_compact_supported' : 'openai_compact_supported']
  return typeof supported === 'boolean' ? (supported ? 'active' : 'blocked') : 'auto'
}
