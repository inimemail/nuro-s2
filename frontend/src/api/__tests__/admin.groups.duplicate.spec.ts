import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({ post: vi.fn() }))

vi.mock('@/api/client', () => ({ apiClient: { post } }))

import { duplicate } from '@/api/admin/groups'

describe('admin groups duplicate api', () => {
  beforeEach(() => post.mockReset())

  it('sends an idempotency key without putting credentials in the request body', async () => {
    post.mockResolvedValue({ data: { id: 12, name: 'Copy' } })
    await expect(duplicate(4)).resolves.toEqual({ id: 12, name: 'Copy' })
    expect(post).toHaveBeenCalledWith(
      '/admin/groups/4/duplicate',
      undefined,
      { headers: { 'Idempotency-Key': expect.stringContaining('group-duplicate-4-') } }
    )
  })
})
