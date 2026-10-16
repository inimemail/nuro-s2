import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'

const routeState = vi.hoisted(() => ({
  query: {} as Record<string, unknown>,
}))
const routerPush = vi.hoisted(() => vi.fn())
const getOrder = vi.hoisted(() => vi.fn())
const paymentStore = vi.hoisted(() => ({
  config: { stripe_publishable_key: 'pk_test' } as { stripe_publishable_key?: string },
  fetchConfig: vi.fn(),
  pollOrderStatus: vi.fn(),
}))
const loadStripe = vi.hoisted(() => vi.fn())
const stripeElements = vi.hoisted(() => ({
  create: vi.fn(),
  getElement: vi.fn(),
}))
const stripePaymentElement = vi.hoisted(() => ({
  mount: vi.fn(),
  on: vi.fn(),
  destroy: vi.fn(),
}))
const stripeInstance = vi.hoisted(() => ({
  elements: vi.fn(),
  confirmPayment: vi.fn(),
  confirmAlipayPayment: vi.fn(),
  confirmWechatPayPayment: vi.fn(),
}))

vi.mock('vue-router', async () => {
  const actual = await vi.importActual<typeof import('vue-router')>('vue-router')
  return {
    ...actual,
    useRoute: () => routeState,
    useRouter: () => ({ push: routerPush }),
  }
})

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
      locale: { value: 'zh-CN' },
    }),
  }
})

vi.mock('@/stores/payment', () => ({
  usePaymentStore: () => paymentStore,
}))

vi.mock('@/api/payment', () => ({
  paymentAPI: {
    getOrder,
  },
}))

vi.mock('@stripe/stripe-js/pure', () => ({
  loadStripe,
}))

import StripePaymentView from '../StripePaymentView.vue'
import { formatPaymentAmount } from '@/components/payment/currency'
import {
  PAYMENT_RECOVERY_STORAGE_KEY,
  type PaymentRecoverySnapshot,
} from '@/components/payment/paymentFlow'
import type { PaymentOrder } from '@/types/payment'

function orderFactory(overrides: Partial<PaymentOrder> = {}): PaymentOrder {
  return {
    id: 42,
    user_id: 7,
    amount: 100,
    pay_amount: 103,
    currency: 'CNY',
    fee_rate: 0.03,
    payment_type: 'stripe',
    out_trade_no: 'sub2_stripe_42',
    status: 'PENDING',
    order_type: 'balance',
    created_at: '2026-04-20T12:00:00Z',
    expires_at: '2026-04-20T12:30:00Z',
    refund_amount: 0,
    ...overrides,
  }
}

function stripeSnapshot(overrides: Partial<PaymentRecoverySnapshot> = {}): PaymentRecoverySnapshot {
  return {
    orderId: 42,
    amount: 100,
    qrCode: '',
    expiresAt: '2099-01-01T00:30:00.000Z',
    paymentType: 'stripe',
    payUrl: '/payment/stripe?order_id=42',
    outTradeNo: 'sub2_stripe_42',
    clientSecret: 'pi_secret_42',
    intentId: '',
    currency: 'CNY',
    countryCode: '',
    paymentEnv: '',
    payAmount: 103,
    orderType: 'balance',
    paymentMode: '',
    resumeToken: '',
    createdAt: Date.UTC(2099, 0, 1, 0, 0, 0),
    ...overrides,
  }
}

function writeSnapshot(overrides: Partial<PaymentRecoverySnapshot> = {}) {
  window.localStorage.setItem(PAYMENT_RECOVERY_STORAGE_KEY, JSON.stringify(stripeSnapshot(overrides)))
}

function mountView() {
  return shallowMount(StripePaymentView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        Icon: true,
      },
    },
  })
}

describe('StripePaymentView', () => {
  beforeEach(() => {
    routeState.query = {
      order_id: '42',
      client_secret: 'pi_secret_42',
    }
    window.localStorage.clear()
    writeSnapshot()
    routerPush.mockReset()
    getOrder.mockReset()
    paymentStore.config = { stripe_publishable_key: 'pk_test' }
    paymentStore.fetchConfig.mockReset().mockResolvedValue(undefined)
    paymentStore.pollOrderStatus.mockReset()
    loadStripe.mockReset().mockResolvedValue(stripeInstance)
    stripeElements.create.mockReset().mockReturnValue(stripePaymentElement)
    stripeElements.getElement.mockReset().mockReturnValue(stripePaymentElement)
    stripePaymentElement.destroy.mockReset()
    stripePaymentElement.mount.mockReset()
    stripePaymentElement.on.mockReset().mockImplementation((event: string, callback: () => void) => {
      if (event === 'ready') callback()
    })
    stripeInstance.elements.mockReset().mockReturnValue(stripeElements)
    stripeInstance.confirmPayment.mockReset()
    stripeInstance.confirmAlipayPayment.mockReset()
    stripeInstance.confirmWechatPayPayment.mockReset()
  })

  it('使用订单接口返回的 Stripe 币种展示金额', async () => {
    getOrder.mockResolvedValue({
      data: orderFactory({ currency: 'HKD', pay_amount: 103 }),
    })

    const wrapper = mountView()
    await flushPromises()
    await flushPromises()

    expect(getOrder).toHaveBeenCalledWith(42)
    expect(loadStripe).toHaveBeenCalledWith('pk_test')
    expect(wrapper.text()).toContain(formatPaymentAmount(103, 'HKD', 'zh-CN'))
    wrapper.unmount()
    expect(stripePaymentElement.destroy).toHaveBeenCalledOnce()
  })

  it('does not load the payment SDK after the order request resolves on an unmounted page', async () => {
    let resolveOrder!: (value: { data: PaymentOrder }) => void
    getOrder.mockReturnValue(new Promise(resolve => { resolveOrder = resolve }))
    const wrapper = mountView()
    wrapper.unmount()
    resolveOrder({ data: orderFactory() })
    await flushPromises()
    expect(loadStripe).not.toHaveBeenCalled()
    expect(paymentStore.fetchConfig).not.toHaveBeenCalled()
  })

  it('does not mount a stale SDK after its order query changes', async () => {
    let resolveStripe!: (value: typeof stripeInstance) => void
    getOrder.mockResolvedValue({ data: orderFactory() })
    loadStripe.mockReturnValue(new Promise(resolve => { resolveStripe = resolve }))
    const wrapper = mountView()
    await flushPromises()
    routeState.query = { order_id: '43', client_secret: 'other' }
    resolveStripe(stripeInstance)
    await flushPromises()
    expect(stripeElements.create).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('rejects a URL secret without a matching browser snapshot', async () => {
    window.localStorage.clear()
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.text()).toContain('payment.stripeMissingParams')
    expect(getOrder).not.toHaveBeenCalled()
    expect(loadStripe).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('ignores a spoofed URL secret and binds to the matching snapshot', async () => {
    routeState.query.client_secret = 'pi_attacker_secret'
    getOrder.mockResolvedValue({ data: orderFactory() })
    const wrapper = mountView()
    await flushPromises()
    expect(stripeInstance.elements).toHaveBeenCalledWith(expect.objectContaining({ clientSecret: 'pi_secret_42' }))
    wrapper.unmount()
  })
  it('rejects a snapshot belonging to another order', async () => {
    writeSnapshot({ orderId: 43 })
    const wrapper = mountView()
    await flushPromises()
    expect(getOrder).not.toHaveBeenCalled()
    expect(loadStripe).not.toHaveBeenCalled()
    wrapper.unmount()
  })

})
