import { describe, expect, it } from 'vitest'
import { accountHomepageUrl } from '../accountHomepage'

describe('accountHomepageUrl', () => {
  it('returns only the origin for API-key accounts', () => {
    expect(accountHomepageUrl({
      type: 'apikey',
      credentials: { base_url: 'https://user:secret@example.com/v1?token=hidden' }
    })).toBe('https://example.com')
  })

  it('does not expose links for non API-key or invalid URLs', () => {
    expect(accountHomepageUrl({ type: 'oauth', credentials: { base_url: 'https://example.com' } })).toBe('')
    expect(accountHomepageUrl({ type: 'apikey', credentials: { base_url: 'javascript:alert(1)' } })).toBe('')
    expect(accountHomepageUrl({ type: 'apikey', credentials: { base_url: '/internal' } })).toBe('')
  })
})
