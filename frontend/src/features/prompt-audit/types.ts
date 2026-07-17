export type PromptAuditMode = 'off' | 'async_audit'

export interface PromptAuditEndpoint {
  id: string
  name: string
  base_url: string
  model: string
  timeout_ms: number
  enabled: boolean
  has_token: boolean
  token_status: 'configured' | 'not_configured' | string
  allow_private: boolean
  allowed_cidrs?: string[]
}

export interface PromptAuditConfig {
  enabled: boolean
  mode: PromptAuditMode
  worker_count: number
  queue_capacity: number
  all_groups: boolean
  group_ids: number[]
  scanners: string[]
  endpoints: PromptAuditEndpoint[]
  store_pass_events: boolean
  retention_days: number
  version: number
  updated_at: string
}

export interface PromptAuditEndpointUpdate {
  id: string
  name: string
  base_url: string
  model: string
  token?: string
  clear_token: boolean
  timeout_ms: number
  enabled: boolean
  allow_private: boolean
  allowed_cidrs: string[]
}

export interface PromptAuditConfigUpdate {
  enabled: boolean
  worker_count: number
  queue_capacity: number
  all_groups: boolean
  group_ids: number[]
  scanners: string[]
  endpoints: PromptAuditEndpointUpdate[]
  store_pass_events: boolean
  retention_days: number
  expected_version: number
}

export interface PromptAuditRuntime {
  mode: PromptAuditMode
  worker_count: number
  queue_capacity: number
  queue_length: number
  enqueued: number
  dropped: number
  processed: number
  failed: number
  last_error?: string
  updated_at: string
}

export interface PromptAuditProbeResult {
  ok: boolean
  status: 'healthy' | 'failed' | string
  error_code?: string
  latency_ms: number
  checked_at: string
}

export interface PromptAuditEvent {
  id: number
  request_id: string
  user_id: number
  user_email: string
  api_key_id: number
  api_key_name: string
  group_id?: number
  group_name: string
  provider: string
  endpoint: string
  protocol: string
  model: string
  prompt_hash: string
  redacted_preview: string
  prompt_length: number
  message_count: number
  stage: string
  decision: string
  risk_level: string
  action: string
  categories: string[]
  scanner_backend: string
  scanner_version: string
  guard_endpoint_id: string
  latency_ms: number
  error_code?: string
  created_at: string
}

export interface PromptAuditEventFilter {
  page: number
  page_size: number
  decision?: string
  risk_level?: string
  group_id?: number
  user_id?: number
  search?: string
}

export interface PromptAuditEventList {
  items: PromptAuditEvent[]
  total: number
  page: number
  page_size: number
}

export interface PromptAuditDeletePreview {
  count: number
  max_id: number
  expires_at: string
  confirmation_token: string
}

export interface PromptAuditEndpointForm extends PromptAuditEndpointUpdate {
  has_token: boolean
  token_status: string
  allowed_cidrs_text: string
}

export interface PromptAuditConfigForm {
  enabled: boolean
  worker_count: number
  queue_capacity: number
  all_groups: boolean
  group_ids: number[]
  scanners: string[]
  endpoints: PromptAuditEndpointForm[]
  store_pass_events: boolean
  retention_days: number
  expected_version: number
}
