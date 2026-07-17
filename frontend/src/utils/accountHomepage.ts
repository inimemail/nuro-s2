import { sanitizeUrl } from '@/utils/url'

/**
 * Return a safe administrator-only homepage link for an API-key account.
 *
 * Account base URLs can contain a path, query, or accidentally supplied
 * userinfo.  Only the origin is rendered so the table never turns credentials
 * or routing details into a clickable link.
 */
export function accountHomepageUrl(account: {
  type?: string | null
  credentials?: Record<string, unknown> | null
}): string {
  if (account.type !== 'apikey') return ''
  const raw = account.credentials?.base_url
  if (typeof raw !== 'string') return ''
  const sanitized = sanitizeUrl(raw)
  if (!sanitized) return ''
  try {
    const parsed = new URL(sanitized)
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return ''
    return parsed.origin
  } catch {
    return ''
  }
}
