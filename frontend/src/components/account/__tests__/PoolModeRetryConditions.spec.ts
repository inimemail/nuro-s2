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

import PoolModeRetryConditions from '../PoolModeRetryConditions.vue'

describe('PoolModeRetryConditions', () => {
  it('adds, validates, and removes explicit HTTP status codes', async () => {
    const wrapper = mount(PoolModeRetryConditions, {
      props: {
        modelValue: [401, 429],
        showBuiltinTransient: true,
        builtinTransientEnabled: true
      }
    })

    await wrapper.get('[data-testid="pool-retry-status-code-input"]').setValue('503')
    await wrapper.get('[data-testid="pool-retry-add-status-code"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')?.at(-1)?.[0]).toEqual([401, 429, 503])

    await wrapper.setProps({ modelValue: [401, 429, 503] })
    expect(wrapper.get('[data-testid="pool-retry-overlap-hint"]').exists()).toBe(true)

    await wrapper.get('[data-testid="pool-retry-remove-status-code-401"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')?.at(-1)?.[0]).toEqual([429, 503])

    await wrapper.get('[data-testid="pool-retry-status-code-input"]').setValue('99')
    await wrapper.get('[data-testid="pool-retry-add-status-code"]').trigger('click')
    expect(wrapper.get('[data-testid="pool-retry-status-code-error"]').text()).toBe('admin.accounts.invalidErrorCode')
  })

  it('toggles the independent transient-system-error rule', async () => {
    const wrapper = mount(PoolModeRetryConditions, {
      props: {
        modelValue: [503],
        showBuiltinTransient: true,
        builtinTransientEnabled: true
      }
    })

    await wrapper.get('[data-testid="pool-mode-builtin-retry-toggle"]').trigger('click')

    expect(wrapper.emitted('update:builtinTransientEnabled')?.at(-1)?.[0]).toBe(false)
  })
})
