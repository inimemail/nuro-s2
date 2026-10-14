import type { GeminiCachedContent, GeminiCacheCreate } from '@/api/geminiCache'

export function cacheIsActive(item: GeminiCachedContent, now = Date.now()) {
  return item.state === 'active' && Date.parse(item.expireTime) > now
}
export function cacheStatus(item: GeminiCachedContent, now = Date.now()) {
  if (item.state === 'active' && Date.parse(item.expireTime) <= now) return 'expired'
  if (item.state === 'reconcile_required') return 'reconcile'
  if (item.state !== 'active') return 'pending'
  return 'active'
}
export function cacheTimeLeft(expiry: string, now = Date.now()) {
  const seconds = Math.max(0, Math.ceil((Date.parse(expiry) - now) / 1000))
  if (!Number.isFinite(seconds)) return '—'
  return `${Math.floor(seconds / 3600)}:${String(Math.floor(seconds % 3600 / 60)).padStart(2, '0')}:${String(seconds % 60).padStart(2, '0')}`
}
export function cacheCreateBody(model: string, text: string, hours: number, displayName: string): GeminiCacheCreate | null {
  const normalized = model.trim().replace(/^models\//, '')
  if (!/^gemini-[A-Za-z0-9._-]+$/.test(normalized) || !text.trim() || !Number.isFinite(hours) || hours < 1 / 60 || hours > 24 || displayName.length > 128) return null
  return { model: `models/${normalized}`, displayName: displayName.trim(), contents: [{ role: 'user', parts: [{ text }] }], ttl: `${Math.round(hours * 3600)}s` }
}
export function cacheMoney(value?: string) {
  const amount = Number(value)
  return Number.isFinite(amount) && amount >= 0 ? `$${amount.toFixed(8)}` : '—'
}

export function cacheOperationID() {
  const bytes = new Uint8Array(16)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
}
