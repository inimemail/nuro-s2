type ErrorShape = { code?: unknown; reason?: unknown }

const messages: Record<string, { zh: string; en: string }> = {
  PROMPT_AUDIT_REQUEST_INVALID: { zh: '配置请求无效', en: 'The configuration request is invalid.' },
  PROMPT_AUDIT_MODE_REQUIRED: { zh: '启用时必须使用异步审计模式', en: 'Enabled audits must use asynchronous mode.' },
  PROMPT_AUDIT_WORKERS_INVALID: { zh: 'Worker 数量必须在 1 到 16 之间', en: 'Worker count must be between 1 and 16.' },
  PROMPT_AUDIT_QUEUE_INVALID: { zh: '队列容量必须在 1 到 100000 之间', en: 'Queue capacity must be between 1 and 100000.' },
  PROMPT_AUDIT_RETENTION_INVALID: { zh: '保留天数必须在 1 到 90 之间', en: 'Retention must be between 1 and 90 days.' },
  PROMPT_AUDIT_GROUPS_REQUIRED: { zh: '请至少选择一个分组', en: 'Select at least one group.' },
  PROMPT_AUDIT_ENDPOINT_REQUIRED: { zh: '请至少启用一个 Guard 端点', en: 'Enable at least one Guard endpoint.' },
  PROMPT_AUDIT_ENDPOINT_INVALID: { zh: 'Guard 端点的 ID、名称和模型为必填项', en: 'Guard endpoint ID, name, and model are required.' },
  PROMPT_AUDIT_ENDPOINT_DUPLICATE: { zh: 'Guard 端点 ID 不能重复', en: 'Guard endpoint IDs must be unique.' },
  PROMPT_AUDIT_TIMEOUT_INVALID: { zh: '端点超时必须在 100 到 30000 ms 之间', en: 'Endpoint timeout must be between 100 and 30000 ms.' },
  PROMPT_AUDIT_ENDPOINT_URL_INVALID: { zh: 'Guard 端点地址无效', en: 'The Guard endpoint address is invalid.' },
  PROMPT_AUDIT_ENDPOINT_SCHEME_INVALID: { zh: 'Guard 端点仅支持 HTTP(S)', en: 'Guard endpoints must use HTTP(S).' },
  PROMPT_AUDIT_ENDPOINT_HTTPS_REQUIRED: { zh: '公网 Guard 端点必须使用 HTTPS', en: 'Public Guard endpoints must use HTTPS.' },
  PROMPT_AUDIT_ENDPOINT_URL_UNSAFE: { zh: 'Guard 端点地址不能包含认证信息、查询参数或锚点', en: 'The Guard endpoint address contains unsupported components.' },
  PROMPT_AUDIT_PRIVATE_ALLOWLIST_REQUIRED: { zh: '私网端点必须配置 CIDR 白名单', en: 'Private endpoints require an explicit CIDR allowlist.' },
  PROMPT_AUDIT_CIDR_INVALID: { zh: 'CIDR 白名单包含无效项', en: 'The CIDR allowlist contains an invalid entry.' },
  PROMPT_AUDIT_CONFIG_CONFLICT: { zh: '配置已被其他管理员更新，已重新加载', en: 'The configuration changed elsewhere and has been reloaded.' },
  PROMPT_AUDIT_DELETE_CONFIRMATION_INVALID: { zh: '删除确认已过期，请重新预览', en: 'The deletion confirmation expired. Preview it again.' },
  STEP_UP_TOTP_NOT_ENABLED: { zh: '请先为管理员账号启用 TOTP', en: 'Enable TOTP for the administrator account first.' },
  STEP_UP_ADMIN_API_KEY_FORBIDDEN: { zh: '管理员 API Key 不能执行此操作', en: 'Administrator API keys cannot perform this operation.' }
}

export function promptAuditErrorCode(error: unknown): string {
  const value = (error || {}) as ErrorShape
  for (const candidate of [value.code, value.reason]) {
    if (typeof candidate === 'string') return candidate
  }
  return ''
}

// Do not render arbitrary server/network error bodies here. They may contain an
// upstream URL, an HTML error page, or other internal diagnostics.
export function promptAuditErrorMessage(error: unknown, locale: string, fallback: string): string {
  const known = messages[promptAuditErrorCode(error)]
  if (!known) return fallback
  return locale.toLowerCase().startsWith('zh') ? known.zh : known.en
}
