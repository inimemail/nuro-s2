import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))

vi.mock('@/api/client', () => ({ apiClient: { get } }))

import { getLiveCapability } from '@/api/admin/groups'

describe('admin groups Live capability API', () => {
  beforeEach(() => get.mockReset())

  it('reads the node capability without sending account or cache state', async () => {
    get.mockResolvedValue({ data: { supported: false, reason: 'unsupported runtime' } })

    await expect(getLiveCapability()).resolves.toEqual({
      supported: false,
      reason: 'unsupported runtime'
    })
    expect(get).toHaveBeenCalledWith('/admin/groups/live-capability')
  })
})
