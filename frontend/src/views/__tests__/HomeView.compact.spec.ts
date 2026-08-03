import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, RouterLinkStub } from '@vue/test-utils'

import HomeView from '../HomeView.vue'

const { appStore, authStore } = vi.hoisted(() => ({
  appStore: {
    cachedPublicSettings: {} as Record<string, unknown>,
    siteName: 'Fallback site',
    siteLogo: '',
    docUrl: '',
    publicSettingsLoaded: true,
    fetchPublicSettings: vi.fn(),
  },
  authStore: {
    isAuthenticated: false,
    isAdmin: false,
    user: null as { email?: string } | null,
    checkAuth: vi.fn(),
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => appStore,
  useAuthStore: () => authStore,
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

function mountHome(settings: Record<string, unknown> = {}) {
  appStore.cachedPublicSettings = {
    site_name: 'Test site',
    site_subtitle: 'Test subtitle',
    ...settings,
  }
  return mount(HomeView, {
    global: {
      stubs: {
        RouterLink: RouterLinkStub,
        LocaleSwitcher: { template: '<div data-testid="locale-switcher" />' },
        Icon: { template: '<span data-testid="icon" />' },
      },
    },
  })
}

describe('HomeView compact mode', () => {
  beforeEach(() => {
    authStore.isAuthenticated = false
    authStore.isAdmin = false
    authStore.user = null
    authStore.checkAuth.mockClear()
    appStore.fetchPublicSettings.mockClear()
    localStorage.clear()
    vi.spyOn(window, 'matchMedia').mockReturnValue({ matches: false } as MediaQueryList)
  })

  it('keeps custom content ahead of compact mode', () => {
    const wrapper = mountHome({
      compact_home_enabled: true,
      home_content: '<section id="custom-home">Custom home</section>',
    })
    expect(wrapper.get('#custom-home').text()).toBe('Custom home')
    expect(wrapper.find('[data-testid="compact-home"]').exists()).toBe(false)
  })

  it('treats whitespace custom content as empty', () => {
    const wrapper = mountHome({ compact_home_enabled: true, home_content: ' \n\t ' })
    expect(wrapper.get('[data-testid="compact-home"]').text()).toContain('Test site')
  })

  it.each([undefined, false])('keeps the default home when compact mode is %s', (enabled) => {
    const wrapper = mountHome(enabled === undefined ? {} : { compact_home_enabled: enabled })
    expect(wrapper.find('[data-testid="compact-home"]').exists()).toBe(false)
    expect(wrapper.find('.terminal-container').exists()).toBe(true)
  })

  it('keeps the default terminal within narrow viewports', () => {
    const wrapper = mountHome()
    const container = wrapper.get('.terminal-container')
    const terminal = wrapper.get('.terminal-window')

    expect(wrapper.get('main').classes()).toContain('min-w-0')
    expect(container.classes()).toEqual(expect.arrayContaining(['w-full', 'max-w-[420px]']))
    expect(terminal.classes()).toContain('w-full')
  })

  it('uses the correct login and dashboard destinations', () => {
    let wrapper = mountHome({ compact_home_enabled: true })
    expect(wrapper.get('[data-testid="compact-home"]').findComponent(RouterLinkStub).props('to')).toBe('/login')

    authStore.isAuthenticated = true
    authStore.isAdmin = true
    wrapper = mountHome({ compact_home_enabled: true })
    expect(wrapper.get('[data-testid="compact-home"]').findComponent(RouterLinkStub).props('to')).toBe('/admin/dashboard')
  })
})
