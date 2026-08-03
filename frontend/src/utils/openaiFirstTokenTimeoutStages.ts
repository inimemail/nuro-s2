export interface OpenAIApiKeyFirstTokenTimeoutStage {
  stage: number
  placeholder_ms: number
  guard_max_ms: number
}

export interface OpenAIApiKeyFirstTokenTimeoutStageConfig {
  stages: OpenAIApiKeyFirstTokenTimeoutStage[]
}

const DEFAULT_PLACEHOLDER_MS = 1000
const DEFAULT_GUARD_MAX_MS = 3000
const MAX_PLACEHOLDER_MS = 3000
const MAX_GUARD_MAX_MS = 30000

function integer(value: unknown): number | null {
  const parsed = Number(value)
  return Number.isInteger(parsed) ? parsed : null
}

function normalizeLegacyScalar(value: unknown, fallback: number, maximum: number): number {
  const parsed = Math.trunc(Number(value))
  if (!Number.isFinite(parsed) || parsed <= 0) return fallback
  return Math.min(maximum, Math.max(1, parsed))
}

export function readOpenAIApiKeyFirstTokenTimeoutStageConfig(
  extra: Record<string, unknown> | undefined
): OpenAIApiKeyFirstTokenTimeoutStageConfig {
  const hasScalarPlaceholder = extra?.openai_apikey_first_token_timeout_placeholder_ms !== undefined
  const hasScalarGuard = extra?.openai_apikey_first_token_timeout_placeholder_guard_max_ms !== undefined
  const scalarPlaceholder = normalizeLegacyScalar(
    extra?.openai_apikey_first_token_timeout_placeholder_ms,
    DEFAULT_PLACEHOLDER_MS,
    MAX_PLACEHOLDER_MS
  )
  const scalarGuard = normalizeLegacyScalar(
    extra?.openai_apikey_first_token_timeout_placeholder_guard_max_ms,
    DEFAULT_GUARD_MAX_MS,
    MAX_GUARD_MAX_MS
  )
  const raw = extra?.openai_apikey_first_token_timeout_placeholder_stages
  if (!Array.isArray(raw) || raw.length < 1 || raw.length > 10) {
    return { stages: [{ stage: 1, placeholder_ms: scalarPlaceholder, guard_max_ms: scalarGuard }] }
  }
  const stages = raw.map((value, index) => {
    const item = value && typeof value === 'object' ? value as Record<string, unknown> : {}
    return {
      stage: index + 1,
      placeholder_ms: integer(item.placeholder_ms) ?? scalarPlaceholder,
      guard_max_ms: integer(item.guard_max_ms) ?? scalarGuard
    }
  })
  // The legacy scalars remain runtime-authoritative so existing bulk edits and
  // older nodes keep their exact behavior. Reflect such edits back into stage
  // one when reopening the per-account editor.
  if (hasScalarPlaceholder) stages[0].placeholder_ms = scalarPlaceholder
  if (hasScalarGuard) stages[0].guard_max_ms = scalarGuard
  return { stages }
}

export function validateOpenAIApiKeyFirstTokenTimeoutStageConfig(
  config: OpenAIApiKeyFirstTokenTimeoutStageConfig,
  translate: (key: string, params?: Record<string, unknown>) => string
): string | null {
  const key = (name: string, params?: Record<string, unknown>) => translate(`admin.accounts.openai.firstTokenTimeoutStages.${name}`, params)
  if (config.stages.length < 1 || config.stages.length > 10) return key('countInvalid')
  for (let index = 0; index < config.stages.length; index += 1) {
    const stage = config.stages[index]
    if (!Number.isInteger(stage.placeholder_ms) || stage.placeholder_ms < 1 || stage.placeholder_ms > 3000) return key('stagePlaceholderInvalid', { stage: index + 1 })
    if (!Number.isInteger(stage.guard_max_ms) || stage.guard_max_ms < 1 || stage.guard_max_ms > 30000) return key('stageGuardInvalid', { stage: index + 1 })
    if (stage.guard_max_ms < stage.placeholder_ms) return key('stageGuardBelow', { stage: index + 1 })
    if (index > 0) {
      const previous = config.stages[index - 1]
      if (stage.placeholder_ms <= previous.placeholder_ms || stage.guard_max_ms <= previous.guard_max_ms) {
        return key('stageOrderInvalid', { stage: index + 1, previous: index })
      }
    }
  }
  return null
}
