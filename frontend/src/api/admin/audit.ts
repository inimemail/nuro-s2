import { apiClient } from '../client'

export interface AuditLog {
  id: number
  created_at: string
  actor_user_id?: number
  actor_email: string
  actor_role: string
  auth_method: string
  credential_masked: string
  action: string
  method: string
  path: string
  request_id: string
  client_ip: string
  user_agent: string
  request_body?: string
  status_code: number
  latency_ms: number
  extra?: Record<string, unknown>
}

export interface AuditListParams {
  page?: number
  page_size?: number
  q?: string
  action?: string
  method?: string
  actor_email?: string
  client_ip?: string
  success?: boolean
  start_time?: string
  end_time?: string
}

export interface AuditListResponse {
  items: AuditLog[]
  total: number
  page: number
  page_size: number
  pages: number
}

async function list(params: AuditListParams = {}): Promise<AuditListResponse> {
  const { data } = await apiClient.get<AuditListResponse>('/admin/audit-logs', { params })
  return data
}

async function get(id: number): Promise<AuditLog> {
  const { data } = await apiClient.get<AuditLog>(`/admin/audit-logs/${id}`)
  return data
}

async function clear(totpCode: string): Promise<{ deleted: number }> {
  const { data } = await apiClient.post<{ deleted: number }>('/admin/audit-logs/clear', { totp_code: totpCode })
  return data
}

export default { list, get, clear }
