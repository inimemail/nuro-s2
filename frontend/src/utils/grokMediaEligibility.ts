export type GrokMediaEligibilityState =
  | 'forced_enabled'
  | 'forced_disabled'
  | 'observed_forbidden'
  | 'automatic'

type GrokMediaAccountLike = {
  platform?: string | null
  type?: string | null
  grok_media_eligible?: boolean | null
  grok_media_eligibility_reason?: string | null
  credentials?: Record<string, unknown> | null
  extra?: Record<string, unknown> | null
}

type GrokBillingObservation = {
  status_code?: unknown
  weekly_status_code?: unknown
  monthly_status_code?: unknown
}

function isForbiddenStatus(value: unknown): boolean {
  return Number(value) === 403
}

function hasAuthoritativeBillingOrigin(account: GrokMediaAccountLike): boolean {
  if (account.type !== 'oauth') return false
  const raw = account.credentials?.base_url
  if (typeof raw !== 'string' || raw.trim() === '') return true
  try {
    const parsed = new URL(raw.trim())
    const port = parsed.port
    const path = parsed.pathname.replace(/\/+$/, '')
    return parsed.protocol === 'https:' &&
      parsed.username === '' &&
      parsed.password === '' &&
      parsed.hostname.toLowerCase() === 'cli-chat-proxy.grok.com' &&
      (port === '' || port === '443') &&
      (path === '' || path === '/v1') &&
      parsed.search === '' &&
      parsed.hash === ''
  } catch {
    // The backend falls an invalid OAuth base URL back to the official CLI
    // origin, where a billing 403 is authoritative.
    return true
  }
}

/** Resolve the media-routing state from explicit override first, then probe data. */
export function resolveGrokMediaEligibility(account: GrokMediaAccountLike): GrokMediaEligibilityState | null {
  if (account.platform !== 'grok') return null

  const extra = account.extra || {}
  const override = extra.grok_media_eligible
  if (override === true) return 'forced_enabled'
  if (override === false) return 'forced_disabled'

  // Be compatible with a future flattened admin DTO while keeping the
  // persisted extra override authoritative.
  const reason = typeof account.grok_media_eligibility_reason === 'string'
    ? account.grok_media_eligibility_reason
    : ''
  if (reason === 'override_enabled') return 'forced_enabled'
  if (reason === 'override_disabled') return 'forced_disabled'
  if (reason === 'billing_forbidden') return 'observed_forbidden'

  const billing = extra.grok_billing_snapshot as GrokBillingObservation | undefined
  if (
    hasAuthoritativeBillingOrigin(account) &&
    billing &&
    (
      isForbiddenStatus(billing.status_code) ||
      isForbiddenStatus(billing.weekly_status_code) ||
      isForbiddenStatus(billing.monthly_status_code)
    )
  ) {
    return 'observed_forbidden'
  }

  if (account.grok_media_eligible === false) return 'observed_forbidden'
  return 'automatic'
}
