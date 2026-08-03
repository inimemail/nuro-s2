import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import OpenAIApiKeyFirstTokenTimeoutStages from '../OpenAIApiKeyFirstTokenTimeoutStages.vue'
import type { OpenAIApiKeyFirstTokenTimeoutStageConfig } from '@/utils/openaiFirstTokenTimeoutStages'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

describe('OpenAIApiKeyFirstTokenTimeoutStages', () => {
  it('adds an empty stage for manual input', async () => {
    const wrapper = mount(OpenAIApiKeyFirstTokenTimeoutStages, {
      props: { modelValue: { stages: [{ stage: 1, placeholder_ms: 1000, guard_max_ms: 3000 }] } },
      global: { stubs: { Icon: true } }
    })
    await wrapper.get('[data-testid="add-first-token-stage"]').trigger('click')
    const emitted = wrapper.emitted('update:modelValue')?.at(-1)?.[0] as OpenAIApiKeyFirstTokenTimeoutStageConfig
    expect(emitted.stages).toHaveLength(2)
    expect(emitted.stages[1]).toEqual({ stage: 2, placeholder_ms: null, guard_max_ms: null })
  })

  it('allows adding another empty stage after a stage reaches the placeholder maximum', async () => {
    const wrapper = mount(OpenAIApiKeyFirstTokenTimeoutStages, {
      props: { modelValue: { stages: [
        { stage: 1, placeholder_ms: 800, guard_max_ms: 5000 },
        { stage: 2, placeholder_ms: 100000, guard_max_ms: 200000 }
      ] } },
      global: { stubs: { Icon: true } }
    })

    expect(wrapper.get('[data-testid="add-first-token-stage"]').attributes('disabled')).toBeUndefined()
    await wrapper.get('[data-testid="add-first-token-stage"]').trigger('click')
    const emitted = wrapper.emitted('update:modelValue')?.at(-1)?.[0] as OpenAIApiKeyFirstTokenTimeoutStageConfig
    expect(emitted.stages[2]).toEqual({ stage: 3, placeholder_ms: null, guard_max_ms: null })
  })

  it('keeps add enabled through stage nine and disables it only at ten stages', async () => {
    const nineStages: OpenAIApiKeyFirstTokenTimeoutStageConfig = {
      stages: Array.from({ length: 9 }, (_, index) => ({
        stage: index + 1,
        placeholder_ms: index === 0 ? 800 : null,
        guard_max_ms: index === 0 ? 5000 : null
      }))
    }
    const wrapper = mount(OpenAIApiKeyFirstTokenTimeoutStages, {
      props: { modelValue: nineStages },
      global: { stubs: { Icon: true } }
    })

    expect(wrapper.get('[data-testid="add-first-token-stage"]').attributes('disabled')).toBeUndefined()
    await wrapper.setProps({
      modelValue: {
        stages: [...nineStages.stages, { stage: 10, placeholder_ms: null, guard_max_ms: null }]
      }
    })
    expect(wrapper.get('[data-testid="add-first-token-stage"]').attributes('disabled')).toBeDefined()
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
