export interface AccountFilters { platform: string; status: string }
// Keep this in sync with AccountPlatform. Persisted filters must not discard
// valid accounts when an administrator refreshes the page.
const platforms = new Set(['', 'anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'typesafe'])
const statuses = new Set(['', 'active', 'inactive', 'error', 'rate_limited', 'temp_unschedulable', 'unschedulable'])
export function normalizeAccountFilters(value: unknown): AccountFilters {
  const source = value && typeof value === 'object' ? value as Record<string, unknown> : {}
  return {
    platform: typeof source.platform === 'string' && platforms.has(source.platform) ? source.platform : '',
    status: typeof source.status === 'string' && statuses.has(source.status) ? source.status : ''
  }
}
export function loadAccountFilters(scope: string | null): AccountFilters {
  try { return normalizeAccountFilters(scope ? JSON.parse(localStorage.getItem(`account-filters:v1:${scope}`) || '{}') : {}) }
  catch { return normalizeAccountFilters(null) }
}
export function saveAccountFilters(scope: string | null, value: unknown): void {
  if (!scope) return
  try { localStorage.setItem(`account-filters:v1:${scope}`, JSON.stringify(normalizeAccountFilters(value))) } catch { /* Private mode may disable storage. */ }
}
