import { describe, expect, it } from 'vitest'
import { formatRechargeMultiplier } from '../rechargeBonus'
import { PROVIDER_CONFIG_FIELDS } from '@/components/payment/providerConfig'

describe('recharge precision and refund configuration', () => {
  it('formats the multiplier with two to six decimals', () => {
    for (const [value, text] of [[1, '1.00'], [1.1, '1.10'], [1.123456, '1.123456'], [0.000001, '0.000001']] as const) expect(formatRechargeMultiplier(value)).toBe(text)
    for (const value of [0, -1, NaN, Infinity]) expect(formatRechargeMultiplier(value)).toBe('1.00')
  })
  it('keeps EasyPay refund success code explicit and defaults to 1', () => {
    const field = PROVIDER_CONFIG_FIELDS.easypay!.find(f => f.key === 'refundSuccessCode')!
    expect(field.defaultValue).toBe('1'); expect(field.options!.map(o => o.value)).toEqual(['1', '0'])
    for (const [provider, fields] of Object.entries(PROVIDER_CONFIG_FIELDS)) if (provider !== 'easypay') expect(fields.some(f => f.key === 'refundSuccessCode')).toBe(false)
  })
})
