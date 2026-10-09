import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { reactive } from 'vue'
import PricingEntryCard from '../PricingEntryCard.vue'
import type { PricingFormEntry } from '../types'

const mocks = vi.hoisted(() => ({ pricing: vi.fn() }))
vi.mock('@/api/admin/channels', () => ({ default: { getModelDefaultPricing: mocks.pricing } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const emptyEntry = (): PricingFormEntry => ({
  models: [], billing_mode: 'token', input_price: null, output_price: null,
  cache_write_price: null, cache_write_1h_price: null, cache_read_price: null,
  image_input_price: null, image_output_price: null, per_request_price: null, intervals: []
})

describe('default pricing response ownership', () => {
  let resolve: (value: { found: boolean; input_price: number }) => void
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.pricing.mockReturnValue(new Promise(done => { resolve = done }))
  })
  const mountCard = (onUpdate = vi.fn()) => mount(PricingEntryCard, {
    props: { entry: emptyEntry(), platform: 'openai', onUpdate },
    global: { stubs: { ModelTagInput: { template: '<button data-testid="add-model" @click="$emit(\'update:models\', [\'gpt-test\'])">add</button>' }, Select: true, Icon: true, IntervalRow: true } }
  })

  it('fills the same empty entry after its model update', async () => {
    const wrapper = mountCard()
    await wrapper.get('[data-testid="add-model"]').trigger('click')
    const submitted = wrapper.emitted('update')![0]![0] as PricingFormEntry
    await wrapper.setProps({ entry: reactive(submitted) })
    resolve({ found: true, input_price: 0.000002 })
    await flushPromises()
    expect(wrapper.emitted('update')).toHaveLength(2)
    expect((wrapper.emitted('update')![1]![0] as PricingFormEntry).input_price).toBe(2)
    wrapper.unmount()
  })

  it('rejects a response after an identical row replaces the original', async () => {
    const wrapper = mountCard()
    await wrapper.get('[data-testid="add-model"]').trigger('click')
    const submitted = wrapper.emitted('update')![0]![0] as PricingFormEntry
    await wrapper.setProps({ entry: submitted })
    await wrapper.setProps({ entry: { ...submitted, models: [...submitted.models] } })
    resolve({ found: true, input_price: 0.000002 })
    await flushPromises()
    expect(wrapper.emitted('update')).toHaveLength(1)
    wrapper.unmount()
  })

  it('preserves an existing reasoning effort multiplier', async () => {
    const wrapper = mountCard()
    await wrapper.setProps({ entry: { ...emptyEntry(), max_reasoning_effort_multiplier: 2 } })
    await wrapper.get('[data-testid="add-model"]').trigger('click')
    expect(mocks.pricing).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('does not replace a manual price entered while the request is pending', async () => {
    const wrapper = mountCard()
    await wrapper.get('[data-testid="add-model"]').trigger('click')
    const submitted = wrapper.emitted('update')![0]![0] as PricingFormEntry
    const entry = reactive(submitted)
    await wrapper.setProps({ entry })
    entry.input_price = 0
    resolve({ found: true, input_price: 0.000002 })
    await flushPromises()
    expect(wrapper.emitted('update')).toHaveLength(1)
    expect(entry.input_price).toBe(0)
    wrapper.unmount()
  })

  it('ignores a response after switching the entry platform', async () => {
    const wrapper = mountCard()
    await wrapper.get('[data-testid="add-model"]').trigger('click')
    const submitted = wrapper.emitted('update')![0]![0] as PricingFormEntry
    await wrapper.setProps({ entry: submitted, platform: 'anthropic' })
    resolve({ found: true, input_price: 0.000002 })
    await flushPromises()
    expect(wrapper.emitted('update')).toHaveLength(1)
    wrapper.unmount()
  })

  it('ignores a response after unmounting', async () => {
    const onUpdate = vi.fn()
    const wrapper = mountCard(onUpdate)
    await wrapper.get('[data-testid="add-model"]').trigger('click')
    const submitted = wrapper.emitted('update')![0]![0] as PricingFormEntry
    await wrapper.setProps({ entry: submitted })
    wrapper.unmount()
    resolve({ found: true, input_price: 0.000002 })
    await flushPromises()
    expect(onUpdate).toHaveBeenCalledOnce()
  })
})
