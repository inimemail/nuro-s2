import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import AccountSchedulingStrategyField from '../AccountSchedulingStrategyField.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

describe('AccountSchedulingStrategyField', () => {
  it('keeps TTFT controls out of strict priority', () => {
    const wrapper = mount(AccountSchedulingStrategyField, {
      props: { modelValue: 'strict_priority' },
    })

    expect(wrapper.find('[data-testid="adaptive-ttft-switch"]').exists()).toBe(false)
    expect(wrapper.findAll('button[aria-pressed]')).toHaveLength(3)
  })

  it('shows the default adaptive threshold and emits updates', async () => {
    const wrapper = mount(AccountSchedulingStrategyField, {
      props: {
        modelValue: 'health_cost_balanced',
        ttftSwitchEnabled: true,
        ttftSwitchThresholdSeconds: 60,
        healthFreshnessMinutes: 15,
      },
    })

    expect(wrapper.get<HTMLInputElement>('[data-testid="adaptive-ttft-threshold"]').element.value).toBe('60')
    expect(wrapper.get<HTMLInputElement>('[data-testid="adaptive-health-freshness"]').element.value).toBe('15')
    await wrapper.get('[data-testid="adaptive-ttft-switch"]').trigger('click')
    await wrapper.get('[data-testid="adaptive-ttft-threshold"]').setValue('30')
    await wrapper.get('[data-testid="adaptive-health-freshness"]').setValue('20')

    expect(wrapper.emitted('update:ttftSwitchEnabled')?.[0]).toEqual([false])
    expect(wrapper.emitted('update:ttftSwitchThresholdSeconds')?.[0]).toEqual([30])
    expect(wrapper.emitted('update:healthFreshnessMinutes')?.[0]).toEqual([20])
  })

  it('does not emit an out-of-range threshold', async () => {
    const wrapper = mount(AccountSchedulingStrategyField, {
      props: { modelValue: 'health_first', ttftSwitchThresholdSeconds: 60 },
    })

    await wrapper.get('[data-testid="adaptive-ttft-threshold"]').setValue('0')
    expect(wrapper.emitted('update:ttftSwitchThresholdSeconds')).toBeUndefined()
  })
})
