import { apiClient } from '@/api/client'
import type {
  PromptAuditConfig,
  PromptAuditConfigUpdate,
  PromptAuditDeletePreview,
  PromptAuditEndpointUpdate,
  PromptAuditEvent,
  PromptAuditEventFilter,
  PromptAuditEventList,
  PromptAuditProbeResult,
  PromptAuditRuntime
} from './types'

const basePath = '/admin/prompt-audit'

function eventQuery(filter: PromptAuditEventFilter): Record<string, string | number> {
  const params: Record<string, string | number> = {
    page: filter.page,
    page_size: filter.page_size
  }
  if (filter.decision) params.decision = filter.decision
  if (filter.risk_level) params.risk_level = filter.risk_level
  if (filter.group_id) params.group_id = filter.group_id
  if (filter.user_id) params.user_id = filter.user_id
  if (filter.search) params.search = filter.search
  return params
}

export async function getConfig(): Promise<PromptAuditConfig> {
  const { data } = await apiClient.get<PromptAuditConfig>(`${basePath}/config`)
  return data
}

export async function updateConfig(payload: PromptAuditConfigUpdate): Promise<PromptAuditConfig> {
  const { data } = await apiClient.put<PromptAuditConfig>(`${basePath}/config`, payload)
  return data
}

export async function probeEndpoint(endpoint: PromptAuditEndpointUpdate): Promise<PromptAuditProbeResult> {
  const { data } = await apiClient.post<PromptAuditProbeResult>(`${basePath}/probe`, { endpoint })
  return data
}

export async function getRuntime(): Promise<PromptAuditRuntime> {
  const { data } = await apiClient.get<PromptAuditRuntime>(`${basePath}/runtime`)
  return data
}

export async function listEvents(filter: PromptAuditEventFilter): Promise<PromptAuditEventList> {
  const { data } = await apiClient.get<PromptAuditEventList>(`${basePath}/events`, {
    params: eventQuery(filter)
  })
  return data
}

export async function getEvent(id: number): Promise<PromptAuditEvent> {
  const { data } = await apiClient.get<PromptAuditEvent>(`${basePath}/events/${id}`)
  return data
}

export async function deleteEvent(id: number): Promise<{ deleted: boolean }> {
  const { data } = await apiClient.delete<{ deleted: boolean }>(`${basePath}/events/${id}`)
  return data
}

export async function deleteEvents(ids: number[]): Promise<{ deleted: number }> {
  const { data } = await apiClient.post<{ deleted: number }>(`${basePath}/events/batch-delete`, { ids })
  return data
}

export async function previewDelete(filter: PromptAuditEventFilter): Promise<PromptAuditDeletePreview> {
  const { data } = await apiClient.post<PromptAuditDeletePreview>(`${basePath}/events/delete-preview`, {
    filter
  })
  return data
}

export async function deleteByFilter(confirmationToken: string): Promise<{ deleted: number }> {
  const { data } = await apiClient.post<{ deleted: number }>(`${basePath}/events/delete-by-filter`, {
    confirmation_token: confirmationToken
  })
  return data
}

export const promptAuditAPI = {
  getConfig,
  updateConfig,
  probeEndpoint,
  getRuntime,
  listEvents,
  getEvent,
  deleteEvent,
  deleteEvents,
  previewDelete,
  deleteByFilter
}

export default promptAuditAPI
