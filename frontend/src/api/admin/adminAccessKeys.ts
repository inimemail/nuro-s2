import { apiClient } from '../client'

export interface AccessRule {
  permissions: string[]
  all_resources?: boolean
  account_ids?: number[]
  group_ids?: number[]
  proxy_ids?: number[]
  platforms?: string[]
  account_types?: string[]
  allow_ungrouped?: boolean
}
export interface AccessKeyInput {
  name: string
  description: string
  enabled: boolean
  expires_at: string | null
  ip_allowlist: string[]
  policy: { rules: AccessRule[] }
  rpm: number
  revision: number
}
export interface AccessKey extends AccessKeyInput {
  id: number
  prefix: string
  creator_id: number
  created_at: string
  last_used_at: string | null
  revoked_at: string | null
}
export interface AccessKeyPage {
  items: AccessKey[]
  next_before_id: number
}
const path = '/admin/settings/scoped-admin-keys'
export const adminAccessKeysAPI = {
  async capabilities(signal?: AbortSignal) {
    return (await apiClient.get<{ enabled: boolean; permissions: string[] }>(`${path}/capabilities`, { signal })).data
  },
  async list(signal?: AbortSignal, beforeID = 0) {
    return (await apiClient.get<AccessKeyPage>(path, { signal, params: beforeID ? { before_id: beforeID } : undefined })).data
  },
  async create(input: AccessKeyInput) { return (await apiClient.post<{ key: AccessKey; secret: string }>(path, input)).data },
  async update(id: number, input: AccessKeyInput) { await apiClient.put(`${path}/${id}`, input) },
  async change(id: number, revision: number, action: 'rotate' | 'revoke' | 'delete') {
    const url = `${path}/${id}${action === 'delete' ? '' : `/${action}`}`
    return (action === 'delete'
      ? await apiClient.delete<{ secret: string }>(url, { data: { revision } })
      : await apiClient.post<{ secret: string }>(url, { revision })).data
  }
}
