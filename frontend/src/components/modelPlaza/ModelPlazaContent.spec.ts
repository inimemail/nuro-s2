import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import ModelPlazaContent from './ModelPlazaContent.vue'
import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'

const copyToClipboard = vi.fn().mockResolvedValue(true)

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard }) }))

function tokenModel(overrides: Partial<PlazaModel> = {}): PlazaModel {
  return {
    name: 'claude-sonnet', platform: 'anthropic',
    pricing: {
      billing_mode: 'token', input_price: 3e-6, output_price: 15e-6,
      cache_write_price: 3.75e-6, cache_read_price: 0.3e-6,
      image_output_price: null, per_request_price: null,
      intervals: []
    },
    official_pricing: {
      input_price: 3e-6, output_price: 15e-6, cache_write_price: 3.75e-6,
      cache_write_1h_price: 6e-6, cache_read_price: 0.3e-6
    },
    ...overrides
  }
}

function group(models: PlazaModel[], overrides: Partial<ModelPlazaGroup> = {}): ModelPlazaGroup {
  return {
    id: 1, name: 'Primary', description: '', platform: 'anthropic', subscription_type: 'standard',
    rate_multiplier: 0.5, peak_rate_enabled: false, peak_start: '', peak_end: '', peak_rate_multiplier: 1,
    is_exclusive: false, image_rate_independent: false, image_rate_multiplier: 1, models, ...overrides
  }
}

function mountContent(groups: ModelPlazaGroup[]) {
  return mount(ModelPlazaContent, {
    props: { response: { description: '', groups }, loading: false, error: false, authenticated: true },
    global: { stubs: { Icon: true } }
  })
}

describe('ModelPlazaContent', () => {
  it('shows scaled token, cache and official 1h prices without changing official rates', () => {
    const wrapper = mountContent([group([tokenModel()])])
    const text = wrapper.text()
    expect(text).toContain('$1.50')
    expect(text).toContain('$7.50')
    expect(text).toContain('$3.75')
    expect(text).toContain('1h $6.00')
  })

  it('shows token tiers and per-request/image intervals', () => {
    const tiered = tokenModel({
      name: 'tiered',
      pricing: {
        ...tokenModel().pricing!,
        intervals: [
          { min_tokens: 0, max_tokens: 200000, tier_label: '', input_price: 3e-6, output_price: 15e-6, cache_write_price: null, cache_read_price: null, per_request_price: null },
          { min_tokens: 200000, max_tokens: null, tier_label: '', input_price: 6e-6, output_price: 30e-6, cache_write_price: null, cache_read_price: null, per_request_price: null }
        ]
      }
    })
    const image = tokenModel({
      name: 'image-model', official_pricing: null,
      pricing: {
        billing_mode: 'image', input_price: null, output_price: null, cache_write_price: null, cache_read_price: null,
        image_output_price: 99, per_request_price: null,
        intervals: [{ min_tokens: 0, max_tokens: null, tier_label: 'HD', input_price: null, output_price: null, cache_write_price: null, cache_read_price: null, per_request_price: 0.04 }]
      }
    })
    const text = mountContent([group([tiered, image], { rate_multiplier: 1 })]).text()
    expect(text).toContain('≤200K')
    expect(text).toContain('>200K')
    expect(text).toContain('HD')
    expect(text).toContain('$0.04')
    expect(text).not.toContain('$99.00')
  })

  it('uses the independent image multiplier without changing token pricing', () => {
    const image = tokenModel({
      name: 'image-model', official_pricing: null,
      pricing: {
        billing_mode: 'image', input_price: null, output_price: null, cache_write_price: null, cache_read_price: null,
        image_output_price: null, per_request_price: 0.08, intervals: []
      }
    })
    const text = mountContent([group([tokenModel(), image], {
      rate_multiplier: 2,
      image_rate_independent: true,
      image_rate_multiplier: 0.5
    })]).text()
    expect(text).toContain('$6.00')
    expect(text).toContain('$0.04')
    expect(text).toContain('0.5x')
  })

  it('sorts by official output price and copies the exact model name', async () => {
    copyToClipboard.mockClear()
    const wrapper = mountContent([group([
      tokenModel({ name: 'cheap', official_pricing: { input_price: 1e-6, output_price: 5e-6, cache_write_price: null, cache_read_price: null } }),
      tokenModel({ name: 'expensive', official_pricing: { input_price: 10e-6, output_price: 75e-6, cache_write_price: null, cache_read_price: null } }),
      tokenModel({ name: 'unknown', official_pricing: null })
    ])])
    const names = wrapper.findAll('tbody tr').map(row => row.find('td span').text())
    expect(names).toEqual(['expensive', 'cheap', 'unknown'])
    await wrapper.find('tbody button').trigger('click')
    expect(copyToClipboard).toHaveBeenCalledWith('expensive')
  })

  it('clears a stale group selection when the platform changes', async () => {
    const wrapper = mountContent([
      group([tokenModel()], { id: 1, name: 'Anthropic', platform: 'anthropic' }),
      group([tokenModel({ name: 'gpt-5', platform: 'openai' })], { id: 2, name: 'OpenAI', platform: 'openai' })
    ])
    const selects = wrapper.findAll('select')
    await selects[1].setValue('1')
    expect(wrapper.text()).toContain('Anthropic')

    await selects[0].setValue('openai')
    expect((selects[1].element as HTMLSelectElement).value).toBe('all')
    expect(wrapper.text()).toContain('OpenAI')
    expect(wrapper.text()).toContain('gpt-5')
  })
})
