import { describe, expect, it } from 'vitest'
import { formatDateLocalInput } from '../format'

describe('formatDateLocalInput', () => {
  it('uses local calendar fields instead of UTC date fields', () => {
    const localDate = new Date(2026, 6, 13, 0, 30)
    expect(formatDateLocalInput(localDate)).toBe('2026-07-13')
  })

  it('returns an empty value for invalid dates', () => {
    expect(formatDateLocalInput(new Date('invalid'))).toBe('')
  })
})
