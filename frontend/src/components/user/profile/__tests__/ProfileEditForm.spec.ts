import { beforeEach, describe, expect, it, vi } from 'vitest'
import { reactive } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import ProfileEditForm from '../ProfileEditForm.vue'

const mocks = vi.hoisted(() => ({ update: vi.fn(), auth: vi.fn(), success: vi.fn(), error: vi.fn() }))
vi.mock('@/api', () => ({ userAPI: { updateProfile: mocks.update } }))
vi.mock('@/stores/auth', () => ({ useAuthStore: mocks.auth }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: mocks.success, showError: mocks.error }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('profile save lifecycle', () => {
  const auth = reactive({ user: { id: 1, username: 'original' }, token: 'user-one' })
  let resolve: (value: { id: number; username: string }) => void
  beforeEach(() => {
    vi.clearAllMocks()
    auth.user = { id: 1, username: 'original' }
    auth.token = 'user-one'
    mocks.auth.mockReturnValue(auth)
    mocks.update.mockReturnValue(new Promise(done => { resolve = done }))
  })
  const mountForm = () => mount(ProfileEditForm, { props: { initialUsername: 'original' } })

  it('preserves a new draft typed while saving', async () => {
    const wrapper = mountForm()
    await wrapper.get('input').setValue('submitted')
    await wrapper.get('form').trigger('submit')
    await wrapper.get('input').setValue('new draft')
    resolve({ id: 1, username: 'submitted' })
    await flushPromises()
    expect((wrapper.get('input').element as HTMLInputElement).value).toBe('new draft')
    expect(auth.user.username).toBe('submitted')
    wrapper.unmount()
  })

  it('does not restore the old user after switching login identity', async () => {
    const wrapper = mountForm()
    await wrapper.get('form').trigger('submit')
    auth.user = { id: 2, username: 'second user' }
    auth.token = 'user-two'
    resolve({ id: 1, username: 'stale user' })
    await flushPromises()
    expect(auth.user).toEqual({ id: 2, username: 'second user' })
    expect((wrapper.get('input').element as HTMLInputElement).value).toBe('second user')
    expect(mocks.success).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('ignores a save response after unmounting', async () => {
    const wrapper = mountForm()
    await wrapper.get('form').trigger('submit')
    wrapper.unmount()
    resolve({ id: 1, username: 'stale user' })
    await flushPromises()
    expect(auth.user.username).toBe('original')
    expect(mocks.success).not.toHaveBeenCalled()
  })
})
