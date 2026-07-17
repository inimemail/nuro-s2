import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, put, post, remove } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
  post: vi.fn(),
  remove: vi.fn()
}))

vi.mock('@/api/client', () => ({ apiClient: { get, put, post, delete: remove } }))

import {
  deleteByFilter,
  deleteEvent,
  listEvents,
  previewDelete,
  probeEndpoint,
  updateConfig
} from '../api'

describe('Prompt Audit API', () => {
  beforeEach(() => {
    get.mockReset()
    put.mockReset()
    post.mockReset()
    remove.mockReset()
  })

  it('sends optimistic config version and explicit token semantics', async () => {
    put.mockResolvedValue({ data: { version: 8 } })
    const payload = {
      enabled: true,
      worker_count: 2,
      queue_capacity: 2048,
      all_groups: true,
      group_ids: [],
      scanners: ['jailbreak'],
      endpoints: [{
        id: 'guard-a', name: 'Guard A', base_url: 'https://guard.invalid', model: 'guard',
        clear_token: false, timeout_ms: 3000, enabled: true, allow_private: false, allowed_cidrs: []
      }],
      store_pass_events: false,
      retention_days: 7,
      expected_version: 7
    }

    await updateConfig(payload)

    expect(put).toHaveBeenCalledWith('/admin/prompt-audit/config', payload)
    expect(payload.endpoints[0]).not.toHaveProperty('token')
  })

  it('wraps endpoint probes without adding prompt content', async () => {
    post.mockResolvedValue({ data: { ok: true } })
    const endpoint = {
      id: 'guard-a', name: 'Guard A', base_url: 'https://guard.invalid', model: 'guard',
      token: 'temporary-token', clear_token: false, timeout_ms: 3000, enabled: true,
      allow_private: false, allowed_cidrs: []
    }

    await probeEndpoint(endpoint)

    expect(post).toHaveBeenCalledWith('/admin/prompt-audit/probe', { endpoint })
    expect(post.mock.calls[0][1]).not.toHaveProperty('prompt')
  })

  it('omits empty event filters and uses the server event schema', async () => {
    get.mockResolvedValue({ data: { items: [], total: 0, page: 2, page_size: 20 } })

    await listEvents({ page: 2, page_size: 20, decision: '', risk_level: '', search: '' })

    expect(get).toHaveBeenCalledWith('/admin/prompt-audit/events', {
      params: { page: 2, page_size: 20 }
    })
  })

  it('keeps filter deletion as a preview-token-confirm sequence', async () => {
    post
      .mockResolvedValueOnce({ data: { count: 3, confirmation_token: 'signed-token' } })
      .mockResolvedValueOnce({ data: { deleted: 3 } })
    const filter = { page: 1, page_size: 100, risk_level: 'critical' }

    await previewDelete(filter)
    await deleteByFilter('signed-token')

    expect(post).toHaveBeenNthCalledWith(1, '/admin/prompt-audit/events/delete-preview', { filter })
    expect(post).toHaveBeenNthCalledWith(2, '/admin/prompt-audit/events/delete-by-filter', {
      confirmation_token: 'signed-token'
    })
  })

  it('preserves the boolean contract of single-event deletion', async () => {
    remove.mockResolvedValue({ data: { deleted: true } })

    await expect(deleteEvent(42)).resolves.toEqual({ deleted: true })
    expect(remove).toHaveBeenCalledWith('/admin/prompt-audit/events/42')
  })
})
