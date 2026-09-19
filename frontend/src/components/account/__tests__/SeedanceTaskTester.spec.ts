import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import SeedanceTaskTester from '../SeedanceTaskTester.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers() })

describe('Seedance integration test safety', () => {
  it('can poll after a successful create when cross-origin headers are not exposed', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: true, status: 200, headers: new Headers(), json: async () => ({ id: 'provider-task' }) })
    vi.stubGlobal('fetch', fetch)
    vi.stubGlobal('crypto', { randomUUID: () => 'stable-test-key' })
    const wrapper = mount(SeedanceTaskTester, { props: { model: 'doubao-seedance-test' } })
    await wrapper.get('input[type="password"]').setValue('sk-test-only')
    await wrapper.get('textarea').setValue('a landscape')
    await wrapper.get('input[type="checkbox"]').setValue(true)
    await wrapper.get('button').trigger('click')
    await flushPromises()
    const refresh = wrapper.findAll('button').find((button) => button.text() === 'common.refresh')!
    expect(refresh).toBeDefined()
    await refresh.trigger('click')
    await flushPromises()
    expect(fetch.mock.calls[1]?.[0]).toMatch(/\/api\/v3\/contents\/generations\/tasks\/provider-task$/)
    expect(fetch.mock.calls[1]?.[1]?.method).toBe('GET')
    wrapper.unmount()
  })

  it('requires explicit consent and recovers an unknown submission with the same idempotency key', async () => {
    const fetch = vi.fn().mockRejectedValue(new Error('response lost'))
    vi.stubGlobal('fetch', fetch)
    vi.stubGlobal('crypto', { randomUUID: () => 'stable-test-key' })
    const wrapper = mount(SeedanceTaskTester, { props: { model: 'doubao-seedance-test' } })
    expect(fetch).not.toHaveBeenCalled()
    await wrapper.get('input[type="password"]').setValue('sk-test-only')
    await wrapper.get('textarea').setValue('a landscape')
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    await wrapper.get('input[type="checkbox"]').setValue(true)
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(wrapper.get('input[type="password"]').attributes('disabled')).toBeDefined()
    const recovery = wrapper.findAll('button').find((button) => button.text().includes('seedanceRecoverSubmission'))!
    await recovery.trigger('click')
    await flushPromises()
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(fetch.mock.calls[0]?.[1]?.headers['Idempotency-Key']).toBe('stable-test-key')
    expect(fetch.mock.calls[1]?.[1]?.body).toBe(fetch.mock.calls[0]?.[1]?.body)
    expect(fetch.mock.calls[1]?.[1]?.headers['Idempotency-Key']).toBe('stable-test-key')
    wrapper.unmount()
  })
})
