import { describe, expect, it } from 'vitest'
import { validateChannelTimePricing, validateIntervals, type IntervalFormEntry } from '../types'

function makeInterval(over: Partial<IntervalFormEntry>): IntervalFormEntry {
  return {
    min_tokens: 0,
    max_tokens: null,
    tier_label: '',
    input_price: null,
    output_price: null,
    cache_write_price: null,
    cache_read_price: null,
    per_request_price: null,
    sort_order: 0,
    ...over,
  }
}

describe('validateIntervals', () => {
  describe('token mode', () => {
    it('rejects unbounded interval that is not last', () => {
      const intervals: IntervalFormEntry[] = [
        makeInterval({ min_tokens: 0, max_tokens: null, input_price: 1, output_price: 1 }),
        makeInterval({ min_tokens: 200000, max_tokens: 500000, input_price: 2, output_price: 2 }),
      ]
      expect(validateIntervals(intervals, 'token')).toMatch(/无上限/)
    })

    it('accepts unbounded interval at the end', () => {
      const intervals: IntervalFormEntry[] = [
        makeInterval({ min_tokens: 0, max_tokens: 200000, input_price: 1, output_price: 1 }),
        makeInterval({ min_tokens: 200000, max_tokens: null, input_price: 2, output_price: 2 }),
      ]
      expect(validateIntervals(intervals, 'token')).toBeNull()
    })

    it('rejects overlapping intervals', () => {
      const intervals: IntervalFormEntry[] = [
        makeInterval({ min_tokens: 0, max_tokens: 250000, input_price: 1, output_price: 1 }),
        makeInterval({ min_tokens: 200000, max_tokens: 500000, input_price: 2, output_price: 2 }),
      ]
      expect(validateIntervals(intervals, 'token')).toMatch(/重叠/)
    })

    it('defaults mode to token when omitted', () => {
      const intervals: IntervalFormEntry[] = [
        makeInterval({ min_tokens: 0, max_tokens: null, input_price: 1, output_price: 1 }),
        makeInterval({ min_tokens: 100, max_tokens: 200, input_price: 2, output_price: 2 }),
      ]
      expect(validateIntervals(intervals)).toMatch(/无上限/)
    })
  })

  describe('image / per_request mode', () => {
    it('allows multiple unbounded tiers identified by label', () => {
      const intervals: IntervalFormEntry[] = [
        makeInterval({ tier_label: '1K', per_request_price: 0.04 }),
        makeInterval({ tier_label: '2K', per_request_price: 0.06 }),
        makeInterval({ tier_label: '4K', per_request_price: 0.08 }),
      ]
      expect(validateIntervals(intervals, 'image')).toBeNull()
      expect(validateIntervals(intervals, 'per_request')).toBeNull()
    })

    it('still rejects negative prices', () => {
      const intervals: IntervalFormEntry[] = [
        makeInterval({ tier_label: '1K', per_request_price: -1 }),
      ]
      expect(validateIntervals(intervals, 'image')).toMatch(/不能为负数/)
    })

    it('still rejects max <= min on a single tier', () => {
      const intervals: IntervalFormEntry[] = [
        makeInterval({ tier_label: '1K', min_tokens: 100, max_tokens: 50, per_request_price: 0.04 }),
      ]
      expect(validateIntervals(intervals, 'image')).toMatch(/必须大于/)
    })
  })
})

describe('validateChannelTimePricing', () => {
  it('accepts a valid timezone and non-overlapping windows', () => {
    expect(validateChannelTimePricing({
      timezone: 'Asia/Shanghai',
      periods: [
        { start_time: '09:00', end_time: '12:00', multiplier: 1.25 },
        { start_time: '13:00', end_time: '00:00', multiplier: 1 },
      ],
    })).toBeNull()
  })

  it('rejects invalid timezone, precision, and overlap', () => {
    expect(validateChannelTimePricing({ timezone: 'Not/AZone', periods: [] })).toMatch(/时区/)
    expect(validateChannelTimePricing({ timezone: 'UTC', periods: [{ start_time: '09:00', end_time: '10:00', multiplier: 1.001 }] })).toMatch(/两位小数/)
    expect(validateChannelTimePricing({ timezone: 'UTC', periods: [
      { start_time: '09:00', end_time: '12:00', multiplier: 1 },
      { start_time: '11:00', end_time: '13:00', multiplier: 1 },
    ] })).toMatch(/重叠/)
  })
})
