import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import OpenAIPlanTypeSelect from '../OpenAIPlanTypeSelect.vue'

function mountSelect(modelValue = '') {
  return mount(OpenAIPlanTypeSelect, {
    props: { modelValue },
    global: {
      plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh: { common: {
        selectOption: '请选择', searchPlaceholder: '搜索', noOptionsFound: '无选项'
      } } } })],
      stubs: { teleport: true }
    }
  })
}

describe('OpenAIPlanTypeSelect', () => {
  it('shows automatic detection without requiring a selection or emitting an update', () => {
    const wrapper = mountSelect()
    expect(wrapper.get('.select-value').text()).toBe('自动识别')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    wrapper.unmount()
  })

  it.each([
    ['plus', 'Plus'], ['business', 'Business'], ['enterprise', 'Enterprise'],
    ['edu', 'Edu'], ['future_plan', 'future_plan（当前档位）']
  ])('displays the stored %s tier without replacing it', (value, label) => {
    const wrapper = mountSelect(value)
    expect(wrapper.get('.select-value').text()).toBe(label)
    expect(wrapper.text()).not.toContain('请选择')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    wrapper.unmount()
  })

  it('keeps manual selection and returning to automatic detection functional', async () => {
    const wrapper = mountSelect()
    await wrapper.get('button').trigger('click')
    await wrapper.findAll('[role="option"]').find(option => option.text() === 'Pro')!.trigger('click')
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual(['pro'])
    await wrapper.setProps({ modelValue: 'pro' })
    expect(wrapper.get('.select-value').text()).toBe('Pro')
    await wrapper.get('button').trigger('click')
    await wrapper.findAll('[role="option"]').find(option => option.text() === '自动识别')!.trigger('click')
    expect(wrapper.emitted('update:modelValue')?.[1]).toEqual([''])
    await wrapper.setProps({ modelValue: '' })
    expect(wrapper.get('.select-value').text()).toBe('自动识别')
    wrapper.unmount()
  })
})
