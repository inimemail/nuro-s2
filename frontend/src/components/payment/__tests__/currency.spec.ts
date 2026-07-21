import { describe, expect, it } from 'vitest'

import { currencySymbol, normalizePaymentCurrency } from '../currency'

describe('payment currency display', () => {
  it('keeps configured non-USD currencies distinct', () => {
    expect(currencySymbol('CNY')).toBe('¥')
    expect(currencySymbol('EUR')).toBe('€')
    expect(currencySymbol('HKD')).toBe('HK$')
    expect(currencySymbol('USD')).toBe('$')
  })

  it('falls back to the normalized code for unknown currencies', () => {
    expect(currencySymbol('zar')).toBe('ZAR')
    expect(normalizePaymentCurrency('invalid')).toBe('CNY')
  })
})
