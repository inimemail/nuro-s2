import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AntigravityQuotaWindows from '../AntigravityQuotaWindows.vue'
vi.mock('vue-i18n', async importOriginal => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))

describe('Antigravity quota display', () => {
  it('keeps unknown windows unknown and shows stale source status', () => {
    const wrapper = mount(AntigravityQuotaWindows, {
      props: {
        groups: [{ key: 'argon', name: 'Argon', windows: [{ window: '5h', utilization: null }, { window: 'weekly', utilization: 42 }] }],
        sources: { summary: { stale: true, error: 'refresh_failed' } }
      },
      global: { stubs: { UsageProgressBar: { props: ['utilization'], template: '<span data-test="known">{{ utilization }}</span>' } } }
    })
    expect(wrapper.text()).toContain('upgradeQuota.partial')
    expect(wrapper.text()).toContain('upgradeQuota.stale')
    expect(wrapper.text()).toContain('upgradeQuota.unknown')
    expect(wrapper.get('[data-test=known]').text()).toBe('42')
    wrapper.unmount()
  })
})
