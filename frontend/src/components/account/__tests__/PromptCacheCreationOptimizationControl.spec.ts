import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import zh from '@/i18n/locales/zh'
import en from '@/i18n/locales/en'
import PromptCacheCreationOptimizationControl from '../PromptCacheCreationOptimizationControl.vue'

const localeState = vi.hoisted(() => ({ current: 'zh' }))
// The application and tests use a runtime-only i18n build. Resolve the real
// locale strings directly without requiring an absent message compiler.
vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => {
      const messages = localeState.current === 'zh' ? zh.admin.accounts : en.admin.accounts
      const value = messages[key.split('.').at(-1) as keyof typeof messages]
      return typeof value === 'string' ? value : key
    }
  })
}))

describe('PromptCacheCreationOptimizationControl', () => {
  for (const locale of ['zh', 'en']) {
    it(`shows all-text-model guidance and keeps all four modes selectable (${locale})`, async () => {
      localeState.current = locale
      const wrapper = mount(PromptCacheCreationOptimizationControl, {
        props: { enabled: true, mode: 'input_125' }
      })
      expect(wrapper.text()).toContain(locale === 'zh' ? '所有文本模型及自定义模型别名' : 'all text models and custom model aliases')
      expect(wrapper.text()).toContain(locale === 'zh' ? '保留原始用量' : 'Preserves original usage')
      for (const [index, mode] of ['reduce', 'suppress', 'free', 'input_125'].entries()) {
        const button = wrapper.get(`[data-testid="prompt-cache-creation-mode-${mode}"]`)
        expect(button.text()).toContain('ABCD'[index])
        expect(button.attributes('aria-pressed')).toBe(String(mode === 'input_125'))
        await button.trigger('click')
        expect(wrapper.emitted('update:mode')?.at(-1)).toEqual([mode])
      }
      await wrapper.setProps({ enabled: false })
      expect(wrapper.find('[data-testid="prompt-cache-creation-mode-input_125"]').exists()).toBe(false)
      await wrapper.get('[data-testid="prompt-cache-creation-optimization-toggle"]').trigger('click')
      expect(wrapper.emitted('update:enabled')?.at(-1)).toEqual([true])
    })
  }
})
