import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

import DownstreamCacheMarkupControl from '../DownstreamCacheMarkupControl.vue'

describe('DownstreamCacheMarkupControl', () => {
  it('is compact while disabled and emits an explicit enable action', async () => {
    const wrapper = mount(DownstreamCacheMarkupControl, {
      props: { enabled: false, thresholdTokens: 100000, percent: 0 }
    })

    expect(wrapper.find('[data-testid="downstream-cache-markup-threshold"]').exists()).toBe(false)
    await wrapper.get('[data-testid="downstream-cache-markup-toggle"]').trigger('click')
    expect(wrapper.emitted('update:enabled')?.at(-1)?.[0]).toBe(true)
  })

  it('normalizes non-negative threshold and percentage inputs', async () => {
    const wrapper = mount(DownstreamCacheMarkupControl, {
      props: { enabled: true, thresholdTokens: 100000, percent: 0 }
    })

    await wrapper.get('[data-testid="downstream-cache-markup-threshold"]').setValue('120000.9')
    expect(wrapper.emitted('update:thresholdTokens')?.at(-1)?.[0]).toBe(120000)

    await wrapper.get('[data-testid="downstream-cache-markup-percent"]').setValue('7.125')
    expect(wrapper.emitted('update:percent')?.at(-1)?.[0]).toBe(7.13)
    expect(wrapper.text()).toContain('admin.accounts.downstreamCacheMarkupZeroHint')
  })
})
