import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { describe, expect, it } from 'vitest'
import MonitorQuotaView from '../MonitorQuotaView.vue'

const i18n = createI18n({
  legacy: false,
  locale: 'en',
  messages: {
    en: {
      monitorCommon: { quota: { unavailable: 'Unavailable', resetSoon: 'Soon' } },
    },
  },
})

describe('MonitorQuotaView', () => {
  it('renders tier usage and multiple currency balances without overflowing labels', () => {
    const wrapper = mount(MonitorQuotaView, {
      global: { plugins: [i18n] },
      props: {
        snapshot: {
          success: true,
          plan_level: 'Coding Pro',
          tiers: [
            { window: '5h', label: 'Fast', used_percent: 82, reset_at: '2099-08-24T12:00:00Z' },
          ],
          balances: [
            { currency: 'CNY', balance: 12.34 },
            { currency: 'USD', balance: 5.67 },
          ],
        },
      },
    })

    expect(wrapper.text()).toContain('Coding Pro')
    expect(wrapper.text()).toContain('Fast/5h')
    expect(wrapper.text()).toContain('82%')
    expect(wrapper.text()).toContain('12.34 CNY')
    expect(wrapper.text()).toContain('5.67 USD')
    expect(wrapper.find('[style*="width: 82%"]')).toBeTruthy()
  })

  it('renders a safe unavailable state', () => {
    const wrapper = mount(MonitorQuotaView, {
      global: { plugins: [i18n] },
      props: { snapshot: { success: false, error: 'Quota endpoint failed' } },
    })
    expect(wrapper.text()).toContain('Quota endpoint failed')
  })
})
