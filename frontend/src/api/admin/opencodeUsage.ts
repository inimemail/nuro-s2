import { apiClient } from '../client'
import type { Account } from '@/types'

export interface OpenCodeUsageSnapshot {
  tiers?: { window: string; used_percent: number; reset_at?: string }[]
  fetched_at?: number
  last_attempt_at: number
  next_refresh_at: number
  failure_count?: number
  http_status?: number
  error?: string
}
export interface OpenCodeUsageState {
  account_id: number
  eligible: boolean
  auto_refresh: boolean
  global_enabled: boolean
  source: 'configured' | 'official'
  snapshot?: OpenCodeUsageSnapshot
}
export interface OpenCodeUsageSettings { enabled: boolean; interval_minutes: number; debounce_minutes: number }
export function isOpenCodeUsageAccount(account: Account): boolean {
  if (account.type !== 'apikey') return false
  const credentials = account.credentials as Record<string, unknown> | undefined
  if (account.platform === 'opencode_go') {
    const mode = String(credentials?.account_mode || account.extra?.account_mode || 'go').trim().toLowerCase()
    const billing = String(account.extra?.cn_billing_mode || '').trim()
    const subscription = billing === 'coding_plan' || (!['payg', 'coding_plan'].includes(billing) && ['coding', 'coding_plan'].includes(String(credentials?.account_mode || '').trim()))
    return mode !== 'zen' && subscription
  }
  if (!['openai', 'anthropic', 'kimi', 'zhipu', 'deepseek', 'minimax'].includes(account.platform)) return false
  return ['https://opencode.ai/zen/go', 'https://opencode.ai/zen/go/v1', 'https://opencode.ai/zen/go/anthropic'].includes(String(credentials?.base_url || '').trim().replace(/\/+$/, '').toLowerCase())
}
export const openCodeUsageAPI = {
  async get(id: number) { return (await apiClient.get<OpenCodeUsageState>(`/admin/accounts/${id}/opencode-go-usage`)).data },
  async refresh(id: number) { return (await apiClient.post<OpenCodeUsageState>(`/admin/accounts/${id}/opencode-go-usage/refresh`)).data },
  async configure(id: number, auto_refresh: boolean, source: OpenCodeUsageState['source']) { return (await apiClient.patch<OpenCodeUsageState>(`/admin/accounts/${id}/opencode-go-usage`, { auto_refresh, source })).data },
  async settings() { return (await apiClient.get<OpenCodeUsageSettings>('/admin/accounts/opencode-go-usage/settings')).data },
  async saveSettings(value: OpenCodeUsageSettings) { return (await apiClient.put<OpenCodeUsageSettings>('/admin/accounts/opencode-go-usage/settings', value)).data }
}
