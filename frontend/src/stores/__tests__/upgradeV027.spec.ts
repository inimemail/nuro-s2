import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { usePaymentStore } from '../payment'
import { useAnnouncementStore } from '../announcements'

const mocks = vi.hoisted(() => ({ config: vi.fn(), list: vi.fn(), markRead: vi.fn() }))
vi.mock('@/api/payment', () => ({ paymentAPI: { getConfig: mocks.config } }))
vi.mock('@/api', () => ({ announcementsAPI: { list: mocks.list, markRead: mocks.markRead } }))

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((r) => { resolve = r })
  return { promise, resolve }
}

describe('v027 asynchronous store boundaries', () => {
  beforeEach(() => { setActivePinia(createPinia()); vi.resetAllMocks() })

  it('shares an in-flight payment configuration and permits retries after synchronous failures', async () => {
    const request = deferred<{ data: object }>()
    mocks.config.mockReturnValueOnce(request.promise)
    const store = usePaymentStore()
    const first = store.fetchConfig()
    const second = store.fetchConfig()
    request.resolve({ data: { enabled: true } })
    expect(await first).toEqual({ enabled: true })
    expect(await second).toEqual({ enabled: true })
    expect(mocks.config).toHaveBeenCalledTimes(1)
    mocks.config.mockImplementationOnce(() => { throw new Error('synchronous failure') })
    expect(await store.fetchConfig(true)).toBeNull()
    mocks.config.mockResolvedValueOnce({ data: { enabled: false } })
    expect(await store.fetchConfig(true)).toEqual({ enabled: false })
  })

  it('does not restore the previous user’s announcements after reset', async () => {
    const request = deferred<unknown[]>()
    mocks.list.mockReturnValueOnce(request.promise)
    const store = useAnnouncementStore()
    const pending = store.fetchAnnouncements()
    store.reset()
    request.resolve([{ id: 1, title: 'old session', notify_mode: 'popup' }])
    await pending
    expect(store.announcements).toEqual([])
    expect(store.currentPopup).toBeNull()
    expect(store.loading).toBe(false)
  })

  it('keeps successful read results even when another announcement fails', async () => {
    mocks.list.mockResolvedValueOnce([{ id: 1 }, { id: 2 }])
    mocks.markRead.mockImplementation((id: number) => id === 1 ? Promise.resolve() : Promise.reject(new Error('failed')))
    const store = useAnnouncementStore()
    await store.fetchAnnouncements()
    await expect(store.markAllAsRead()).rejects.toThrow('failed')
    expect(store.announcements[0]?.read_at).toBeTruthy()
    expect(store.announcements[1]?.read_at).toBeUndefined()
    expect(store.loading).toBe(false)
  })
})
