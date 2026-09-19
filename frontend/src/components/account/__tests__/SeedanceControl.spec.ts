import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import SeedanceControl from '../SeedanceControl.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('Seedance opt-in', () => {
  it('starts disabled and displays billing guidance only after explicit opt-in', async () => {
    const wrapper = mount(SeedanceControl, { props: { modelValue: false } })
    expect(wrapper.find('code').exists()).toBe(false)
    await wrapper.get('input').setValue(true)
    expect(wrapper.emitted('update:modelValue')).toEqual([[true]])
    await wrapper.setProps({ modelValue: true })
    expect(wrapper.get('code').text()).toContain('/api/v3/contents/generations/tasks')
    expect(wrapper.text()).toContain('seedanceRequirements')
  })
})
