import type {
  PromptAuditConfig,
  PromptAuditConfigForm,
  PromptAuditConfigUpdate,
  PromptAuditEndpointForm,
  PromptAuditEndpointUpdate,
  PromptAuditEventFilter
} from './types'

export const scannerOptions = [
  'violent',
  'non_violent_illegal_acts',
  'sexual_content_or_sexual_acts',
  'pii',
  'suicide_and_self_harm',
  'unethical_acts',
  'politically_sensitive_topics',
  'copyright_violation',
  'jailbreak'
] as const

export function displayableUpdatedAt(value: string | null | undefined): string {
  if (!value) return ''
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime()) || parsed.getUTCFullYear() <= 1) return ''
  return value
}

export function configToForm(config: PromptAuditConfig): PromptAuditConfigForm {
  return {
    enabled: config.enabled,
    worker_count: config.worker_count,
    queue_capacity: config.queue_capacity,
    all_groups: config.all_groups,
    group_ids: [...(config.group_ids || [])],
    scanners: [...(config.scanners || [])],
    endpoints: (config.endpoints || []).map((endpoint): PromptAuditEndpointForm => ({
      id: endpoint.id,
      name: endpoint.name,
      base_url: endpoint.base_url,
      model: endpoint.model,
      token: '',
      clear_token: false,
      timeout_ms: endpoint.timeout_ms,
      enabled: endpoint.enabled,
      has_token: endpoint.has_token,
      token_status: endpoint.token_status,
      allow_private: endpoint.allow_private,
      allowed_cidrs: [...(endpoint.allowed_cidrs || [])],
      allowed_cidrs_text: (endpoint.allowed_cidrs || []).join('\n')
    })),
    store_pass_events: config.store_pass_events,
    retention_days: config.retention_days,
    expected_version: config.version
  }
}

export function cidrsFromText(value: string): string[] {
  return Array.from(new Set(value.split(/[\n,]/).map((item) => item.trim()).filter(Boolean))).sort()
}

export function endpointToUpdate(endpoint: PromptAuditEndpointForm): PromptAuditEndpointUpdate {
  const result: PromptAuditEndpointUpdate = {
    id: endpoint.id.trim(),
    name: endpoint.name.trim(),
    base_url: endpoint.base_url.trim(),
    model: endpoint.model.trim(),
    clear_token: endpoint.clear_token,
    timeout_ms: Number(endpoint.timeout_ms),
    enabled: endpoint.enabled,
    allow_private: endpoint.allow_private,
    allowed_cidrs: cidrsFromText(endpoint.allowed_cidrs_text)
  }
  const token = endpoint.token?.trim()
  if (token) result.token = token
  return result
}

export function formToUpdate(form: PromptAuditConfigForm): PromptAuditConfigUpdate {
  return {
    enabled: form.enabled,
    worker_count: Number(form.worker_count),
    queue_capacity: Number(form.queue_capacity),
    all_groups: form.all_groups,
    group_ids: form.all_groups ? [] : Array.from(new Set(form.group_ids.map(Number))).filter((id) => id > 0).sort((a, b) => a - b),
    scanners: Array.from(new Set(form.scanners)).sort(),
    endpoints: form.endpoints.map(endpointToUpdate),
    store_pass_events: form.store_pass_events,
    retention_days: Number(form.retention_days),
    expected_version: form.expected_version
  }
}

export function deleteFilter(filter: PromptAuditEventFilter): PromptAuditEventFilter {
  const result: PromptAuditEventFilter = { page: 1, page_size: 100 }
  if (filter.decision) result.decision = filter.decision
  if (filter.risk_level) result.risk_level = filter.risk_level
  if (filter.group_id) result.group_id = filter.group_id
  if (filter.user_id) result.user_id = filter.user_id
  if (filter.search?.trim()) result.search = filter.search.trim()
  return result
}

export function createEndpointForm(id: string): PromptAuditEndpointForm {
  return {
    id,
    name: '',
    base_url: '',
    model: 'Qwen3Guard-Gen-8B',
    token: '',
    clear_token: false,
    timeout_ms: 3000,
    enabled: true,
    has_token: false,
    token_status: 'not_configured',
    allow_private: false,
    allowed_cidrs: [],
    allowed_cidrs_text: ''
  }
}
