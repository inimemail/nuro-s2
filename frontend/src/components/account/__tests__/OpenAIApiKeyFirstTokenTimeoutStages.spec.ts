import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import OpenAIApiKeyFirstTokenTimeoutStages from '../OpenAIApiKeyFirstTokenTimeoutStages.vue'
import type { OpenAIApiKeyFirstTokenTimeoutStageConfig } from '@/utils/openaiFirstTokenTimeoutStages'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

describe('OpenAIApiKeyFirstTokenTimeoutStages', () => {
  it('adds a strictly larger automatic stage', async () => {
    const wrapper = mount(OpenAIApiKeyFirstTokenTimeoutStages, {
      props: { modelValue: { stages: [{ stage: 1, placeholder_ms: 1000, guard_max_ms: 3000 }] } },
      global: { stubs: { Icon: true } }
    })
    await wrapper.get('[data-testid="add-first-token-stage"]').trigger('click')
    const emitted = wrapper.emitted('update:modelValue')?.at(-1)?.[0] as OpenAIApiKeyFirstTokenTimeoutStageConfig
    expect(emitted.stages).toHaveLength(2)
    expect(emitted.stages[1].placeholder_ms).toBeGreaterThan(1000)
    expect(emitted.stages[1].guard_max_ms).toBeGreaterThan(3000)
  })

  it('shows both ordering errors when a later stage is smaller', () => {
    const wrapper = mount(OpenAIApiKeyFirstTokenTimeoutStages, {
      props: { modelValue: { stages: [
        { stage: 1, placeholder_ms: 1500, guard_max_ms: 5000 },
        { stage: 2, placeholder_ms: 1200, guard_max_ms: 4000 }
      ] } },
      global: { stubs: { Icon: true } }
    })
    expect(wrapper.text()).toContain('firstTokenTimeoutStages.placeholderOrder')
    expect(wrapper.text()).toContain('firstTokenTimeoutStages.guardOrder')
  })

  it('marks every later stage invalid after an earlier stage is raised above them', () => {
    const wrapper = mount(OpenAIApiKeyFirstTokenTimeoutStages, {
      props: { modelValue: { stages: [
        { stage: 1, placeholder_ms: 1000, guard_max_ms: 3000 },
        { stage: 2, placeholder_ms: 2500, guard_max_ms: 8000 },
        { stage: 3, placeholder_ms: 1800, guard_max_ms: 6000 },
        { stage: 4, placeholder_ms: 2200, guard_max_ms: 7000 }
      ] } },
      global: { stubs: { Icon: true } }
    })
    expect(wrapper.find('[data-testid="stage-3-placeholder"]').element.closest('.rounded-md')?.textContent).toContain('firstTokenTimeoutStages.placeholderOrder')
    expect(wrapper.find('[data-testid="stage-4-placeholder"]').element.closest('.rounded-md')?.textContent).toContain('firstTokenTimeoutStages.placeholderOrder')
  })

  it('renumbers later stages after deletion', async () => {
    const wrapper = mount(OpenAIApiKeyFirstTokenTimeoutStages, {
      props: { modelValue: { stages: [
        { stage: 1, placeholder_ms: 1000, guard_max_ms: 3000 },
        { stage: 2, placeholder_ms: 1500, guard_max_ms: 5000 },
        { stage: 3, placeholder_ms: 2000, guard_max_ms: 7000 }
      ] } },
      global: { stubs: { Icon: true } }
    })
    await wrapper.get('[data-testid="remove-stage-2"]').trigger('click')
    const emitted = wrapper.emitted('update:modelValue')?.at(-1)?.[0] as OpenAIApiKeyFirstTokenTimeoutStageConfig
    expect(emitted.stages.map((stage) => stage.stage)).toEqual([1, 2])
  })

  it('keeps the original single threshold editor when the guard is disabled', () => {
    const wrapper = mount(OpenAIApiKeyFirstTokenTimeoutStages, {
      props: {
        guardEnabled: false,
        modelValue: { stages: [{ stage: 1, placeholder_ms: 1000, guard_max_ms: 3000 }] }
      },
      global: { stubs: { Icon: true } }
    })
    expect(wrapper.find('[data-testid="single-placeholder"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="add-first-token-stage"]').exists()).toBe(false)
  })
})
