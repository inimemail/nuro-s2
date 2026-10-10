import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import UsageLatencyCell from '../UsageLatencyCell.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

const textRow = { output_tokens: 210, first_token_ms: 734, duration_ms: 8330, image_count: 0, billing_mode: 'token' }

describe('UsageLatencyCell', () => {
  it('always renders three metrics with request-average TPS and its unit', () => {
    const wrapper = mount(UsageLatencyCell, { props: { row: textRow } })
    expect(wrapper.text()).toContain('734ms')
    expect(wrapper.text()).toContain('8.33s')
    expect(wrapper.get('[data-testid="output-tps"]').text()).toBe('25.2 tok/s')
    expect(wrapper.text()).not.toContain('27.6')
  })

  it.each([
    { billing_mode: 'image' }, { billing_mode: 'video' }, { billing_mode: 'per_request' },
    { image_count: 1 }, { image_output_tokens: 10 },
    { video_count: 1 }, { video_count: 1, billing_mode: undefined },
    { duration_ms: undefined }, { duration_ms: 0 }, { duration_ms: -1 },
    { duration_ms: NaN }, { duration_ms: Infinity },
    { output_tokens: undefined }, { output_tokens: -1 },
    { output_tokens: NaN }, { output_tokens: Infinity },
    { output_tokens: Number.MAX_VALUE, duration_ms: Number.MIN_VALUE },
  ])('shows a placeholder for unsupported or invalid data: %j', (override) => {
    const wrapper = mount(UsageLatencyCell, { props: { row: { ...textRow, ...override } } })
    expect(wrapper.get('[data-testid="output-tps"]').text()).toBe('-')
    expect(wrapper.findAll('.grid > span')).toHaveLength(6)
    expect(wrapper.text()).not.toMatch(/NaN|Infinity/)
  })

  it('supports legacy token records, zero output, and missing first-token timing', async () => {
    const wrapper = mount(UsageLatencyCell, { props: { row: { ...textRow, billing_mode: undefined, first_token_ms: undefined } } })
    expect(wrapper.get('[data-testid="output-tps"]').text()).toBe('25.2 tok/s')
    await wrapper.setProps({ row: { ...textRow, output_tokens: 0 } })
    expect(wrapper.get('[data-testid="output-tps"]').text()).toBe('0.0 tok/s')
  })

  it('preserves latency severity colors and long duration formatting', () => {
    const wrapper = mount(UsageLatencyCell, { props: { row: { ...textRow, first_token_ms: 30000, duration_ms: 300000 } } })
    expect(wrapper.text()).toContain('30.00s')
    expect(wrapper.text()).toContain('5m 0s')
    expect(wrapper.find('.text-orange-600').exists()).toBe(true)
    expect(wrapper.find('.text-red-600').exists()).toBe(true)
  })

  it('updates every metric when a refreshed or paginated row reuses the cell', async () => {
    const wrapper = mount(UsageLatencyCell, { props: { row: textRow } })
    await wrapper.setProps({ row: { ...textRow, first_token_ms: 12000, duration_ms: 120000, output_tokens: 120 } })
    expect(wrapper.text()).toContain('12.00s')
    expect(wrapper.text()).toContain('2m 0s')
    expect(wrapper.get('[data-testid="output-tps"]').text()).toBe('1.0 tok/s')
    expect(wrapper.text()).not.toContain('734ms')
    expect(wrapper.text()).not.toContain('25.2 tok/s')
    await wrapper.setProps({ row: { ...textRow, video_count: 1 } })
    expect(wrapper.get('[data-testid="output-tps"]').text()).toBe('-')
    await wrapper.setProps({ row: textRow })
    expect(wrapper.get('[data-testid="output-tps"]').text()).toBe('25.2 tok/s')
  })
})
