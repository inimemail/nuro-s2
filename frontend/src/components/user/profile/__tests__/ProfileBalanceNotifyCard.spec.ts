import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { reactive } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import ProfileBalanceNotifyCard from '../ProfileBalanceNotifyCard.vue'

const mocks = vi.hoisted(() => ({ auth: vi.fn(), send: vi.fn(), verify: vi.fn(), profile: vi.fn(), update: vi.fn(), toggle: vi.fn(), remove: vi.fn(), success: vi.fn(), error: vi.fn() }))
vi.mock('@/api', () => ({ userAPI: { sendNotifyEmailCode: mocks.send, verifyNotifyEmail: mocks.verify, getProfile: mocks.profile, updateProfile: mocks.update, toggleNotifyEmail: mocks.toggle, removeNotifyEmail: mocks.remove } }))
vi.mock('@/stores/auth', () => ({ useAuthStore: mocks.auth }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: mocks.success, showError: mocks.error }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const deferred = () => {
  let resolve!: () => void
  const promise = new Promise<void>(done => { resolve = done })
  return { promise, resolve }
}

describe('balance notification request lifecycle', () => {
  const auth = reactive({ user: { id: 1 }, token: 'first-session' })
  const mountCard = () => mount(ProfileBalanceNotifyCard, { props: {
    enabled: true, threshold: null, systemDefaultThreshold: 5, userEmail: 'owner@example.test',
    extraEmails: [{ email: 'notify@example.test', verified: false, disabled: false }]
  } })
  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    auth.user = { id: 1 }
    auth.token = 'first-session'
    mocks.auth.mockReturnValue(auth)
    mocks.send.mockResolvedValue(undefined)
  })
  afterEach(() => vi.useRealTimers())

  it('ignores verification completion after cancellation', async () => {
    const pending = deferred()
    mocks.verify.mockReturnValueOnce(pending.promise)
    const wrapper = mountCard()
    await wrapper.findAll('button').find(button => button.text() === 'profile.balanceNotify.verify')!.trigger('click')
    await flushPromises()
    await wrapper.get('input[maxlength="6"]').setValue('123456')
    await wrapper.findAll('button').find(button => button.text() === 'profile.balanceNotify.verify')!.trigger('click')
    await wrapper.findAll('button').find(button => button.text() === 'common.cancel')!.trigger('click')
    mocks.success.mockClear()
    pending.resolve()
    await flushPromises()
    expect(mocks.profile).not.toHaveBeenCalled()
    expect(mocks.success).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
    wrapper.unmount()
  })

  it('rejects an old response even when identity changes back to the same user', async () => {
    const pending = deferred()
    mocks.send.mockReturnValueOnce(pending.promise)
    const wrapper = mountCard()
    await wrapper.findAll('button').find(button => button.text() === 'profile.balanceNotify.verify')!.trigger('click')
    auth.user = { id: 2 }
    auth.user = { id: 1 }
    pending.resolve()
    await flushPromises()
    expect(wrapper.find('input[maxlength="6"]').exists()).toBe(false)
    expect(mocks.success).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
    wrapper.unmount()
  })

  it('does not create a countdown from a send response after unmounting', async () => {
    const pending = deferred()
    mocks.send.mockReturnValueOnce(pending.promise)
    const wrapper = mountCard()
    await wrapper.findAll('button').find(button => button.text() === 'profile.balanceNotify.verify')!.trigger('click')
    wrapper.unmount()
    pending.resolve()
    await flushPromises()
    expect(mocks.success).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('blocks overlapping profile writes and unlocks after failure', async () => {
    let reject!: (reason: Error) => void
    mocks.update.mockReturnValueOnce(new Promise((_, fail) => { reject = fail }))
    const wrapper = mountCard()
    await wrapper.get('input[type="checkbox"]').setValue(false)
    expect(wrapper.get('input[type="checkbox"]').attributes('disabled')).toBeDefined()
    expect(mocks.update).toHaveBeenCalledWith({ balance_notify_enabled: false })
    reject(new Error('offline'))
    await flushPromises()
    expect((wrapper.get('input[type="checkbox"]').element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.get('input[type="checkbox"]').attributes('disabled')).toBeUndefined()
    expect(mocks.error).toHaveBeenCalledOnce()
    wrapper.unmount()
  })

  it('keeps a newer threshold draft when the saved profile refreshes', async () => {
    const pending = deferred()
    mocks.update.mockReturnValueOnce(pending.promise.then(() => ({ id: 1 })))
    const wrapper = mountCard()
    await wrapper.get('input[type="number"]').setValue('10')
    await wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')
    await wrapper.get('input[type="number"]').setValue('20')
    pending.resolve()
    await flushPromises()
    await wrapper.setProps({ threshold: 10 })
    expect((wrapper.get('input[type="number"]').element as HTMLInputElement).value).toBe('20')
    wrapper.unmount()
  })

  it('accepts refreshed thresholds once the current draft has been saved', async () => {
    mocks.update.mockResolvedValueOnce({ id: 1 })
    const wrapper = mountCard()
    await wrapper.get('input[type="number"]').setValue('10')
    await wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')
    await flushPromises()
    await wrapper.setProps({ threshold: 30 })
    expect((wrapper.get('input[type="number"]').element as HTMLInputElement).value).toBe('30')
    wrapper.unmount()
  })

  it('discards the threshold draft when switching users', async () => {
    const wrapper = mountCard()
    await wrapper.get('input[type="number"]').setValue('20')
    auth.user = { id: 2 }
    await wrapper.setProps({ threshold: 30 })
    expect((wrapper.get('input[type="number"]').element as HTMLInputElement).value).toBe('30')
    wrapper.unmount()
  })

  it('restores the saved email checkbox after a rejected toggle', async () => {
    mocks.toggle.mockRejectedValueOnce(new Error('offline'))
    const wrapper = mountCard()
    const checkbox = wrapper.findAll('input[type="checkbox"]')[1]!
    await checkbox.setValue(false)
    await flushPromises()
    expect(mocks.toggle).toHaveBeenCalledWith('notify@example.test', true)
    expect((checkbox.element as HTMLInputElement).checked).toBe(true)
    expect(checkbox.attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
})
