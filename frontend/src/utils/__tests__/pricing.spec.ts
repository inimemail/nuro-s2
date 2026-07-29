import { describe, expect, it } from 'vitest'
import { formatScaled } from '../pricing'

describe('formatScaled', () => {
  it('keeps existing decimal formatting and minimum fraction digits', () => {
    expect(formatScaled(0.000003, 1_000_000)).toBe('$3')
    expect(formatScaled(0.000003, 1_000_000, 2)).toBe('$3.00')
    expect(formatScaled(null, 1_000_000, 2)).toBe('-')
  })

  it('does not trim zeroes from scientific notation exponents', () => {
    expect(formatScaled(1e-10, 1)).toBe('$1e-10')
    expect(formatScaled(1.25e-10, 1, 2)).toBe('$1.25e-10')
    expect(formatScaled(1e20, 1)).toBe('$1e+20')
  })
})
