import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import SearchInput from '../SearchInput.vue'

afterEach(() => vi.useRealTimers())
describe('SearchInput composition lifecycle', () => {
  it('cancels an earlier search during IME and emits one completed search', async () => {
    vi.useFakeTimers()
    const wrapper = mount(SearchInput, { props: { modelValue: '', debounceMs: 100 } })
    const input = wrapper.get('input')
    await input.setValue('n')
    await input.trigger('compositionstart')
    input.element.value = 'ni'
    await input.trigger('input')
    await vi.advanceTimersByTimeAsync(150)
    expect(wrapper.emitted('search')).toBeUndefined()
    input.element.value = '你好'
    await input.trigger('compositionend')
    await vi.advanceTimersByTimeAsync(150)
    expect(wrapper.emitted('search')).toEqual([['你好']])
    wrapper.unmount()
  })
  it('does not emit a pending search after unmount', async () => {
    vi.useFakeTimers()
    const wrapper = mount(SearchInput, { props: { modelValue: '' } })
    await wrapper.get('input').setValue('pending')
    wrapper.unmount()
    await vi.runAllTimersAsync()
    expect(wrapper.emitted('search')).toBeUndefined()
  })
})
