import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import ProfilePasskeyCard from '@/components/user/profile/ProfilePasskeyCard.vue'

const { list, register, rename, remove } = vi.hoisted(() => ({
  list: vi.fn(), register: vi.fn(), rename: vi.fn(), remove: vi.fn()
}))

vi.mock('@/api', () => ({
  passkeyAPI: {
    isSupported: () => true,
    list,
    register,
    rename,
    remove
  }
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() })
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

describe('ProfilePasskeyCard', () => {
  beforeEach(() => { vi.clearAllMocks() })

  it('performs no API calls while the server-side feature is disabled', async () => {
    const wrapper = mount(ProfilePasskeyCard, {
      props: { enabled: false },
      global: { stubs: { Icon: true } }
    })
    await flushPromises()

    expect(list).not.toHaveBeenCalled()
    expect(register).not.toHaveBeenCalled()
    expect(rename).not.toHaveBeenCalled()
    expect(remove).not.toHaveBeenCalled()
    expect(wrapper.find('form').exists()).toBe(false)
    expect(wrapper.text()).toContain('profile.passkey.featureDisabled')
    expect(wrapper.text()).not.toContain('profile.passkey.empty')
  })

  it('loads credentials only after the feature becomes enabled', async () => {
    list.mockResolvedValue([])
    const wrapper = mount(ProfilePasskeyCard, {
      props: { enabled: false },
      global: { stubs: { Icon: true } }
    })
    await wrapper.setProps({ enabled: true })
    await flushPromises()

    expect(list).toHaveBeenCalledTimes(1)
  })
})
