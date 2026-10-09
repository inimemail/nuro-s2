import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import OpsSettingsDialog from '../OpsSettingsDialog.vue'

const api = vi.hoisted(() => ({
  getAlertRuntimeSettings: vi.fn(), getEmailNotificationConfig: vi.fn(), getAdvancedSettings: vi.fn(), getMetricThresholds: vi.fn(),
  updateMetricThresholds: vi.fn(), updateAlertRuntimeSettings: vi.fn(), updateEmailNotificationConfig: vi.fn(), updateAdvancedSettings: vi.fn()
}))
vi.mock('@/api/admin/ops', () => ({ opsAPI: api }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('OPS settings save lifecycle', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    api.getAlertRuntimeSettings.mockResolvedValue({ evaluation_interval_seconds: 30 })
    api.getEmailNotificationConfig.mockResolvedValue({ alert: { enabled: false, recipients: [] }, report: { enabled: false, recipients: [] } })
    api.getAdvancedSettings.mockResolvedValue({
      data_retention: { cleanup_enabled: false, error_log_retention_days: 7, minute_metrics_retention_days: 7, hourly_metrics_retention_days: 7 },
      aggregation: { aggregation_enabled: false }, ignore_count_tokens_errors: false, ignore_context_canceled: false,
      ignore_no_available_accounts: false, ignore_insufficient_balance_errors: false, auto_refresh_enabled: false,
      display_alert_events: false, display_openai_token_stats: false
    })
    api.getMetricThresholds.mockResolvedValue({ sla_percent_min: 99.5, ttft_p99_ms_max: 500, request_error_rate_percent_max: 5, upstream_error_rate_percent_max: 5 })
  })

  it('leaves newer threshold input open after a pending save completes', async () => {
    let resolve!: () => void
    api.updateMetricThresholds.mockReturnValueOnce(new Promise<void>(done => { resolve = done }))
    const wrapper = mount(OpsSettingsDialog, {
      props: { show: true },
      global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }, Select: true, Toggle: true } }
    })
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')
    await wrapper.find('input[type="number"]').setValue('97')
    resolve()
    await flushPromises()
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect(wrapper.emitted('close')).toBeUndefined()
    expect((wrapper.find('input[type="number"]').element as HTMLInputElement).value).toBe('97')
    wrapper.unmount()
  })
})
