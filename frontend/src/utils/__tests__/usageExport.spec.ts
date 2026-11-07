import { afterEach, describe, expect, it, vi } from 'vitest'
import { usageExportRetryDelay, waitForUsageExport } from '../usageExport'

afterEach(() => vi.useRealTimers())
describe('usage CSV retry bounds', () => {
  it('accepts seconds and HTTP dates and bounds every wait', () => {
    const now = Date.UTC(2026, 10, 7)
    expect(usageExportRetryDelay('2', 0, now)).toBe(2000)
    expect(usageExportRetryDelay(new Date(now + 3000).toUTCString(), 0, now)).toBe(3000)
    expect(usageExportRetryDelay('999999', 0, now)).toBe(60000)
    expect(usageExportRetryDelay('0', 0, now)).toBe(250)
    expect(usageExportRetryDelay('invalid', 2, now)).toBe(4000)
    expect(usageExportRetryDelay('invalid', 30, now)).toBe(10000)
  })
  it('cancels immediately before or during a wait and releases the timer', async () => {
    vi.useFakeTimers()
    const prior = new AbortController()
    prior.abort()
    await expect(waitForUsageExport(60000, prior.signal)).rejects.toMatchObject({ name: 'AbortError' })
    const active = new AbortController()
    const pending = waitForUsageExport(60000, active.signal)
    const rejected = expect(pending).rejects.toMatchObject({ name: 'AbortError' })
    active.abort()
    await rejected
    expect(vi.getTimerCount()).toBe(0)
  })
  it('removes the abort listener on normal completion', async () => {
    vi.useFakeTimers()
    const active = new AbortController()
    const remove = vi.spyOn(active.signal, 'removeEventListener')
    const pending = waitForUsageExport(250, active.signal)
    await vi.advanceTimersByTimeAsync(250)
    await pending
    expect(remove).toHaveBeenCalledWith('abort', expect.any(Function))
    expect(vi.getTimerCount()).toBe(0)
  })
})
