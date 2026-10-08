import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import SetupWizardView from '../SetupWizardView.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/setup', () => ({ testDatabase: vi.fn().mockResolvedValue({}), testRedis: vi.fn().mockResolvedValue({}), install: vi.fn() }))

describe('SetupWizard admin password', () => {
  it('validates UTF-8 byte boundaries and confirmation before advancing', async () => {
    const wrapper = mount(SetupWizardView, { global: { stubs: { Select: true, Toggle: true, Icon: true } } })
    const clickText = async (text: string) => {
      await wrapper.findAll('button').find(button => button.text().includes(text))!.trigger('click')
      await flushPromises()
    }
    await clickText('setup.status.testConnection')
    await clickText('common.next')
    await clickText('setup.status.testConnection')
    await clickText('common.next')
    await wrapper.get('input[type="email"]').setValue('admin@example.com')
    for (const [password, bytes, valid] of [
      ['1234567', 7, false], ['12345678', 8, true], ['中中中', 9, true],
      ['中'.repeat(24), 72, true], ['中'.repeat(24) + 'x', 73, false]
    ] as const) {
      await wrapper.get('#admin-password').setValue(password)
      await wrapper.findAll('input[type="password"]')[1].setValue(password)
      expect(wrapper.get('#admin-password-status').text()).toContain(`${bytes} / 72`)
      const next = wrapper.findAll('button').find(button => button.text().includes('common.next'))!
      expect(next.attributes('disabled') !== undefined).toBe(!valid)
    }
  })
})
