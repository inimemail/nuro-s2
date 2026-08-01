import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

import PoolModeRaceRetryConditions from '../PoolModeRaceRetryConditions.vue'

describe('PoolModeRaceRetryConditions', () => {
  it('adds new status rules at zero and rejects limits above the shared budget', async () => {
    const wrapper = mount(PoolModeRaceRetryConditions, {
      props: {
        modelValue: [{ matcher: '429', max_retries: 2 }],
        total: 2,
        transportEnabled: true,
        transportRetryCount: 1
      }
    })

    await wrapper.get('[data-testid="race-retry-rule-input"]').setValue('503')
    await wrapper.get('[data-testid="race-retry-add-rule"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')?.at(-1)?.[0]).toEqual([
      { matcher: '429', max_retries: 2 },
      { matcher: '503', max_retries: 0 }
    ])

    await wrapper.setProps({
      modelValue: [
        { matcher: '429', max_retries: 2 },
        { matcher: '503', max_retries: 0 }
      ]
    })
    const emittedBeforeOverBudgetEdit = wrapper.emitted('update:modelValue')?.length
    await wrapper.get('[data-testid="race-retry-limit-503"]').setValue('1')

    expect(wrapper.emitted('update:modelValue')?.length).toBe(emittedBeforeOverBudgetEdit)
    expect(wrapper.get('[data-testid="race-retry-rule-error"]').text()).toBe(
      'admin.accounts.upstreamConcurrencyRaceRuleBudgetExceeded'
    )
    expect((wrapper.get('[data-testid="race-retry-limit-503"]').element as HTMLInputElement).value).toBe('0')
  })

  it('rejects transient-system retries above the total race limit', async () => {
    const wrapper = mount(PoolModeRaceRetryConditions, {
      props: {
        modelValue: [],
        total: 3,
        transportEnabled: true,
        transportRetryCount: 1
      }
    })

    await wrapper.get('[data-testid="race-retry-transport-count"]').setValue('4')

    expect(wrapper.emitted('update:transportRetryCount')).toBeUndefined()
    expect(wrapper.get('[data-testid="race-retry-transport-error"]').text()).toBe(
      'admin.accounts.upstreamConcurrencyRaceTransportBudgetExceeded'
    )
    expect((wrapper.get('[data-testid="race-retry-transport-count"]').element as HTMLInputElement).value).toBe('1')
  })
})
