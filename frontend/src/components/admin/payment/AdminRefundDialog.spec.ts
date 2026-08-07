import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import AdminRefundDialog from './AdminRefundDialog.vue'
import type { PaymentOrder } from '@/types/payment'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const order = {
  id: 17,
  amount: 100,
  pay_amount: 100,
  currency: 'USD',
  status: 'COMPLETED'
} as PaymentOrder

describe('AdminRefundDialog', () => {
  it('drops the force requirement when balance deduction is disabled', async () => {
    const wrapper = mount(AdminRefundDialog, {
      props: { show: false, order, requireForce: true },
      global: {
        stubs: {
          BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }
        }
      }
    })
    await wrapper.setProps({ show: true })

    const submit = wrapper.get('button[type="submit"]')
    expect(submit.attributes('disabled')).toBeDefined()
    expect(wrapper.find('#force-refund').exists()).toBe(true)

    await wrapper.get('#deduct-balance').setValue(false)
    expect(wrapper.find('#force-refund').exists()).toBe(false)
    expect(submit.attributes('disabled')).toBeUndefined()

    await wrapper.get('form').trigger('submit')
    expect(wrapper.emitted('confirm')?.[0]?.[0]).toMatchObject({
      amount: 100,
      deduct_balance: false,
      force: false
    })
  })
})
