import { describe, expect, it } from 'vitest'

import { getExpirationDateRelation, getRemainingExpiryDuration } from '../subscriptionQuota'

describe('subscription quota expiry helpers', () => {
  const now = new Date(2026, 6, 19, 10, 0, 0)

  it('classifies expiration by local calendar date', () => {
    expect(getExpirationDateRelation(new Date(2026, 6, 19, 23, 59), now)).toBe('today')
    expect(getExpirationDateRelation(new Date(2026, 6, 20, 9, 0), now)).toBe('tomorrow')
    expect(getExpirationDateRelation(new Date(2026, 6, 18, 23, 59), now)).toBe('expired')
  })

  it('formats short and long remaining durations without rounding down', () => {
    expect(getRemainingExpiryDuration(new Date(2026, 6, 19, 10, 1, 1), now)).toEqual({
      unit: 'hoursMinutes',
      hours: 0,
      minutes: 2,
    })
    expect(getRemainingExpiryDuration(new Date(2026, 6, 20, 10, 0), now)).toEqual({
      unit: 'days',
      days: 1,
    })
  })

  it('returns null for invalid or expired values', () => {
    expect(getExpirationDateRelation('not-a-date', now)).toBeNull()
    expect(getRemainingExpiryDuration(new Date(2026, 6, 19, 9, 59), now)).toBeNull()
  })
})
