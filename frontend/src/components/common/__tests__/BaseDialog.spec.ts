import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import BaseDialog from '../BaseDialog.vue'

afterEach(() => vi.restoreAllMocks())

function create(trapFocus: boolean) {
  vi.spyOn(HTMLElement.prototype, 'getClientRects').mockReturnValue([{}] as unknown as DOMRectList)
  return mount(BaseDialog, {
    attachTo: document.body,
    props: { show: true, title: 'Review dialog', trapFocus },
    slots: { default: '<input aria-label="Name" /><button disabled>Disabled</button>', footer: '<button>Save</button>' },
    global: { stubs: { Icon: true } }
  })
}

describe('BaseDialog optional keyboard containment', () => {
  it('wraps focus in both directions and skips disabled controls', async () => {
    const wrapper = create(true)
    await flushPromises()
    const close = document.querySelector<HTMLButtonElement>('[aria-label="Close modal"]')!
    const save = document.querySelector<HTMLButtonElement>('.modal-footer button')!
    close.focus()
    const reverse = new KeyboardEvent('keydown', { key: 'Tab', shiftKey: true, cancelable: true })
    document.dispatchEvent(reverse)
    expect(reverse.defaultPrevented).toBe(true)
    expect(document.activeElement).toBe(save)
    const forward = new KeyboardEvent('keydown', { key: 'Tab', cancelable: true })
    document.dispatchEvent(forward)
    expect(forward.defaultPrevented).toBe(true)
    expect(document.activeElement).toBe(close)
    wrapper.unmount()
  })

  it('leaves existing dialogs unchanged when the option is disabled', async () => {
    const wrapper = create(false)
    await flushPromises()
    const reverse = new KeyboardEvent('keydown', { key: 'Tab', shiftKey: true, cancelable: true })
    document.dispatchEvent(reverse)
    expect(reverse.defaultPrevented).toBe(false)
    wrapper.unmount()
  })

  it.each([false, true])('contains Tab from the panel itself (shiftKey=%s)', async (shiftKey) => {
    const wrapper = create(true)
    try {
      await flushPromises()
      const panel = document.querySelector<HTMLElement>('.modal-content')!
      const close = document.querySelector<HTMLButtonElement>('[aria-label="Close modal"]')!
      const save = document.querySelector<HTMLButtonElement>('.modal-footer button')!
      panel.focus()
      expect(document.activeElement).toBe(panel)
      const event = new KeyboardEvent('keydown', { key: 'Tab', shiftKey, cancelable: true })
      document.dispatchEvent(event)
      expect(event.defaultPrevented).toBe(true)
      expect(document.activeElement).toBe(shiftKey ? save : close)
    } finally {
      wrapper.unmount()
    }
  })

  it('only contains focus in the top dialog', async () => {
    const first = create(true)
    const second = create(true)
    await flushPromises()
    const closes = document.querySelectorAll<HTMLButtonElement>('[aria-label="Close modal"]')
    const saves = document.querySelectorAll<HTMLButtonElement>('.modal-footer button')
    closes[1].focus()
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', shiftKey: true, cancelable: true }))
    expect(document.activeElement).toBe(saves[1])
    second.unmount()
    first.unmount()
  })
})
