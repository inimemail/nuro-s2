import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({ post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { post } }))

describe('account duplication request', () => {
  beforeEach(() => { post.mockReset(); sessionStorage.clear(); vi.resetModules() })

  it('keeps the same retry identity after network failure and module reload, then clears it on success', async () => {
    const { duplicate } = await import('@/api/admin/accounts')
    post.mockRejectedValueOnce(new Error('connection lost'))
    await expect(duplicate(42)).rejects.toThrow('connection lost')
    const firstKey = post.mock.calls[0][2].headers['Idempotency-Key']
    expect(post.mock.calls[0][1]).toBeUndefined()
    expect(sessionStorage.getItem('sub2api:admin:account-duplicate:42')).toBe(firstKey)
    vi.resetModules()
    const reloaded = await import('@/api/admin/accounts')
    post.mockResolvedValue({ data: { id: 43, name: 'source (Copy)' } })
    await expect(reloaded.duplicate(42)).resolves.toMatchObject({ id: 43 })
    expect(post.mock.calls[1][2].headers['Idempotency-Key']).toBe(firstKey)
    expect(sessionStorage.getItem('sub2api:admin:account-duplicate:42')).toBeNull()
    await reloaded.duplicate(42)
    expect(post.mock.calls[2][2].headers['Idempotency-Key']).not.toBe(firstKey)
    expect(post.mock.calls[2][0]).toBe('/admin/accounts/42/duplicate')
  })

  it('retains retry protection when browser storage is unavailable', async () => {
    const getter = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('blocked') })
    const setter = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('blocked') })
    try {
      const { duplicate } = await import('@/api/admin/accounts')
      post.mockRejectedValueOnce(new Error('timeout'))
      await expect(duplicate(1)).rejects.toThrow('timeout')
      post.mockResolvedValueOnce({ data: { id: 2 } })
      await duplicate(1)
      expect(post.mock.calls[1][2]).toEqual(post.mock.calls[0][2])
    } finally { getter.mockRestore(); setter.mockRestore() }
  })
})
