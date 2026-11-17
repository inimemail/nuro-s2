import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import EndpointDistributionChart from '../EndpointDistributionChart.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/dashboard', () => ({ getUserBreakdown: vi.fn() }))
vi.mock('vue-chartjs', () => ({ Doughnut: { props: ['data'], template: '<div class="chart-data">{{ JSON.stringify(data) }}</div>' } }))

describe('endpoint cost meanings', () => {
  const row = { endpoint: '/v1/responses', requests: 2, total_tokens: 1000, cost: 8, actual_cost: 3, account_cost: 0 }
  it('keeps actual debit in the chart and displays account cost independently, including zero', () => {
    const wrapper = mount(EndpointDistributionChart, { props: { endpointStats: [row], metric: 'actual_cost' } })
    const data = JSON.parse(wrapper.get('.chart-data').text())
    expect(data.datasets[0].data).toEqual([3])
    const cells = wrapper.findAll('tbody tr')[0]!.findAll('td')
    expect(cells).toHaveLength(6)
    expect(cells[3]!.text()).toBe('$3.00')
    expect(cells[4]!.text()).toBe('$8.00')
    expect(cells[5]!.text()).toBe('$0.0000')
    wrapper.unmount()
  })
  it('supports older responses without the optional account cost column', () => {
    const wrapper = mount(EndpointDistributionChart, { props: { endpointStats: [{ ...row, account_cost: undefined }] } })
    expect(wrapper.findAll('thead th')).toHaveLength(5)
    wrapper.unmount()
  })
  it('uses the selected endpoint dimension without mixing account costs', async () => {
    const wrapper = mount(EndpointDistributionChart, { props: { endpointStats: [row], upstreamEndpointStats: [{ ...row, actual_cost: 5, account_cost: 1 }], metric: 'actual_cost' } })
    await wrapper.setProps({ source: 'upstream' })
    const data = JSON.parse(wrapper.get('.chart-data').text())
    expect(data.datasets[0].data).toEqual([5])
    expect(wrapper.findAll('tbody tr')[0]!.findAll('td')[5]!.text()).toBe('$1.00')
    wrapper.unmount()
  })
})
