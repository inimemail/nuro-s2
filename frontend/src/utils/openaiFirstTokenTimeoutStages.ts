export interface OpenAIApiKeyFirstTokenTimeoutStage {
  stage: number
  placeholder_ms: number | null
  guard_max_ms: number | null
}

export interface OpenAIApiKeyFirstTokenTimeoutStageConfig {
  stages: OpenAIApiKeyFirstTokenTimeoutStage[]
}

const DEFAULT_PLACEHOLDER_MS = 1000
const DEFAULT_GUARD_MAX_MS = 3000
const MAX_PLACEHOLDER_MS = 100000

export function createDefaultOpenAIApiKeyFirstTokenTimeoutStageConfig(): OpenAIApiKeyFirstTokenTimeoutStageConfig {
  return {
    stages: [
      { stage: 1, placeholder_ms: 800, guard_max_ms: 5000 },
      { stage: 2, placeholder_ms: 3000, guard_max_ms: 10000 },
      { stage: 3, placeholder_ms: 5000, guard_max_ms: 15000 },
      { stage: 4, placeholder_ms: 10000, guard_max_ms: 30000 }
    ]
  }
}

function integer(value: unknown): number | null {
  const parsed = Number(value)
  return Number.isInteger(parsed) ? parsed : null
}

function normalizeLegacyScalar(value: unknown, fallback: number, maximum?: number): number {
  const parsed = Math.trunc(Number(value))
  if (!Number.isFinite(parsed) || parsed <= 0) return fallback
  const normalized = Math.max(1, parsed)
  return maximum === undefined ? normalized : Math.min(maximum, normalized)
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
    DEFAULT_GUARD_MAX_MS
  )
  const raw = extra?.openai_apikey_first_token_timeout_placeholder_stages
  if (!Array.isArray(raw) || raw.length < 1 || raw.length > 10) {
    if (!hasScalarPlaceholder && !hasScalarGuard) {
      return createDefaultOpenAIApiKeyFirstTokenTimeoutStageConfig()
    }
    return {
      stages: [{
        stage: 1,
        placeholder_ms: scalarPlaceholder,
        guard_max_ms: Math.max(scalarPlaceholder, scalarGuard)
      }]
    }
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

export type OpenAIOAuthFirstTokenTimeoutStage = OpenAIApiKeyFirstTokenTimeoutStage
export type OpenAIOAuthFirstTokenTimeoutStageConfig = OpenAIApiKeyFirstTokenTimeoutStageConfig

export function createDefaultOpenAIOAuthFirstTokenTimeoutStageConfig(): OpenAIOAuthFirstTokenTimeoutStageConfig {
  return createDefaultOpenAIApiKeyFirstTokenTimeoutStageConfig()
}

export function readOpenAIOAuthFirstTokenTimeoutStageConfig(
  extra: Record<string, unknown> | undefined
): OpenAIOAuthFirstTokenTimeoutStageConfig {
  const rawStages = extra?.openai_oauth_chatgpt_first_token_timeout_placeholder_stages
  let legacyGuard = extra?.openai_oauth_chatgpt_first_token_timeout_placeholder_guard_max_ms
  if (Array.isArray(rawStages) && rawStages.length > 1) {
    const first = rawStages[0] && typeof rawStages[0] === 'object'
      ? rawStages[0] as Record<string, unknown>
      : undefined
    const lastValue = rawStages[rawStages.length - 1]
    const last = lastValue && typeof lastValue === 'object'
      ? lastValue as Record<string, unknown>
      : undefined
    const firstGuard = integer(first?.guard_max_ms)
    const lastGuard = integer(last?.guard_max_ms)
    const scalarGuard = integer(legacyGuard)
    if (firstGuard !== null && lastGuard !== null && firstGuard !== lastGuard && scalarGuard === lastGuard) {
      legacyGuard = firstGuard
    }
  }
  const mapped = extra && {
    ...extra,
    openai_apikey_first_token_timeout_placeholder_ms: extra.openai_oauth_chatgpt_first_token_timeout_placeholder_ms,
    openai_apikey_first_token_timeout_placeholder_guard_max_ms: legacyGuard,
    openai_apikey_first_token_timeout_placeholder_stages: rawStages
  }
  const config = readOpenAIApiKeyFirstTokenTimeoutStageConfig(mapped)
  return config
}

export function validateOpenAIApiKeyFirstTokenTimeoutStageConfig(
  config: OpenAIApiKeyFirstTokenTimeoutStageConfig,
  translate: (key: string, params?: Record<string, unknown>) => string
): string | null {
  const key = (name: string, params?: Record<string, unknown>) => translate(`admin.accounts.openai.firstTokenTimeoutStages.${name}`, params)
  if (config.stages.length < 1 || config.stages.length > 10) return key('countInvalid')
  for (let index = 0; index < config.stages.length; index += 1) {
    const stage = config.stages[index]
    const placeholder = stage.placeholder_ms
    const guard = stage.guard_max_ms
    if (typeof placeholder !== 'number' || !Number.isInteger(placeholder) || placeholder < 1 || placeholder > 100000) return key('stagePlaceholderInvalid', { stage: index + 1 })
    if (typeof guard !== 'number' || !Number.isSafeInteger(guard) || guard < 1) return key('stageGuardInvalid', { stage: index + 1 })
    if (guard < placeholder) return key('stageGuardBelow', { stage: index + 1 })
    if (index > 0) {
      const previous = config.stages[index - 1]
      const previousPlaceholder = previous.placeholder_ms
      const previousGuard = previous.guard_max_ms
      if (
        typeof previousPlaceholder !== 'number' ||
        typeof previousGuard !== 'number' ||
        placeholder <= previousPlaceholder ||
        guard <= previousGuard
      ) {
        return key('stageOrderInvalid', { stage: index + 1, previous: index })
      }
    }
  }
  return null
}
