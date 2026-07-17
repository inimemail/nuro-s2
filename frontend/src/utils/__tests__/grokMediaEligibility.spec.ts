import { describe, expect, it } from 'vitest'
import { resolveGrokMediaEligibility } from '../grokMediaEligibility'

describe('resolveGrokMediaEligibility', () => {
  it('gives an explicit override precedence over a stale forbidden probe', () => {
    expect(resolveGrokMediaEligibility({
      platform: 'grok',
      type: 'oauth',
      extra: {
        grok_media_eligible: true,
        grok_billing_snapshot: { weekly_status_code: 403 }
      }
    })).toBe('forced_enabled')
  })

  it('marks an authoritative billing window 403 as unavailable', () => {
    expect(resolveGrokMediaEligibility({
      platform: 'grok',
      type: 'oauth',
      extra: { grok_billing_snapshot: { monthly_status_code: 403 } }
    })).toBe('observed_forbidden')
  })

  it('keeps missing and transient observations automatic', () => {
    expect(resolveGrokMediaEligibility({ platform: 'grok', type: 'oauth', extra: {} })).toBe('automatic')
    expect(resolveGrokMediaEligibility({
      platform: 'grok',
      type: 'oauth',
      extra: { grok_billing_snapshot: { status_code: 502 } }
    })).toBe('automatic')
  })

  it('does not quarantine API-key or custom OAuth accounts from non-authoritative 403 observations', () => {
    const extra = { grok_billing_snapshot: { status_code: 403 } }
    expect(resolveGrokMediaEligibility({ platform: 'grok', type: 'apikey', extra })).toBe('automatic')
    expect(resolveGrokMediaEligibility({
      platform: 'grok',
      type: 'oauth',
      credentials: { base_url: 'https://relay.example/v1' },
      extra
    })).toBe('automatic')
  })

  it('does not produce a badge for another platform', () => {
    expect(resolveGrokMediaEligibility({ platform: 'openai', type: 'apikey', extra: {} })).toBeNull()
  })
})
