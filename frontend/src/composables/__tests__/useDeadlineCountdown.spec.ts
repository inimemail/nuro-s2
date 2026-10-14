import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'
import { useDeadlineCountdown } from '../useDeadlineCountdown'

afterEach(() => vi.useRealTimers())

describe('absolute payment deadline', () => {
  function harness() {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-13T00:00:00Z'))
    const expired = vi.fn()
    let countdown!: ReturnType<typeof useDeadlineCountdown>
    const wrapper = mount(defineComponent({ setup() { countdown = useDeadlineCountdown(expired); return () => null } }))
    return { countdown, expired, wrapper }
  }

  it('catches up after a suspended tab and expires only once', () => {
    const { countdown, expired, wrapper } = harness()
    countdown.start('2026-10-13T00:01:00Z', 1)
    expect(countdown.remainingSeconds.value).toBe(60)
    vi.setSystemTime(new Date('2026-10-13T00:00:40Z'))
    document.dispatchEvent(new Event('visibilitychange'))
    expect(countdown.remainingSeconds.value).toBe(20)
    vi.advanceTimersByTime(20000)
    expect(expired).toHaveBeenCalledTimes(1)
    vi.advanceTimersByTime(10000)
    expect(expired).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('retains the fallback deadline on reopen and clears resources on unmount', () => {
    const { countdown, expired, wrapper } = harness()
    countdown.start('', 1)
    vi.advanceTimersByTime(600000)
    countdown.stop()
    countdown.start('', 1)
    expect(countdown.remainingSeconds.value).toBe(1200)
    countdown.start('2026-10-13T00:10:01Z', 2)
    expect(vi.getTimerCount()).toBe(1)
    wrapper.unmount()
    expect(vi.getTimerCount()).toBe(0)
    vi.advanceTimersByTime(5000)
    document.dispatchEvent(new Event('visibilitychange'))
    expect(expired).not.toHaveBeenCalled()
  })

  it('rejects invalid server deadlines without creating a fresh lifetime', () => {
    const { countdown, expired, wrapper } = harness()
    countdown.start('invalid', 1)
    expect(countdown.remainingSeconds.value).toBe(0)
    expect(expired).toHaveBeenCalledTimes(1)
    expect(vi.getTimerCount()).toBe(0)
    wrapper.unmount()
  })
})
