import { apiClient } from '../client'

export interface CacheReconcileRow {
  operationId: string
  resourceName: string
  userId: number
  apiKeyId: number
  groupId: number
  kind: string
  state: string
  attempts: number
  errorCode: string
  reservedAmount: string
  confirmedTokens: number | null
  actualAmount: string
  deficitAmount: string
}
export async function listReconcile(pageToken = '', signal?: AbortSignal) {
  const { data } = await apiClient.get<{ items: CacheReconcileRow[]; nextPageToken: string }>('/admin/gemini-cache/reconcile', { params: { pageToken }, signal })
  return data
}
export async function reconcileCache(row: CacheReconcileRow, action: 'recheck' | 'reserve-deficit', reason: string) {
  await apiClient.post(`/admin/gemini-cache/reconcile/${row.operationId}/${action}`, { reason, expectedAmount: row.actualAmount })
}
