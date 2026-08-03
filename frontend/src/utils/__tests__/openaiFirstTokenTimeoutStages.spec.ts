import { describe, expect, it } from 'vitest'
import { readOpenAIApiKeyFirstTokenTimeoutStageConfig } from '../openaiFirstTokenTimeoutStages'

describe('readOpenAIApiKeyFirstTokenTimeoutStageConfig', () => {
  it('uses legacy timeout scalars as stage one values after a bulk edit', () => {
    const config = readOpenAIApiKeyFirstTokenTimeoutStageConfig({
      openai_apikey_first_token_timeout_placeholder_stages: [
        { stage: 1, placeholder_ms: 1000, guard_max_ms: 3000 },
        { stage: 2, placeholder_ms: 1500, guard_max_ms: 5000 }
      ],
      openai_apikey_first_token_timeout_placeholder_ms: 1200,
      openai_apikey_first_token_timeout_placeholder_guard_max_ms: 4000
    })

    expect(config.stages[0]).toEqual({ stage: 1, placeholder_ms: 1200, guard_max_ms: 4000 })
    expect(config.stages[1]).toEqual({ stage: 2, placeholder_ms: 1500, guard_max_ms: 5000 })
  })

  it('does not read the independent safe-placeholder switch', () => {
    const config = readOpenAIApiKeyFirstTokenTimeoutStageConfig({
      openai_apikey_safe_token_placeholder_enabled: true,
      openai_apikey_first_token_timeout_placeholder_ms: 900,
      openai_apikey_first_token_timeout_placeholder_guard_max_ms: 2800
    })

    expect(config).toEqual({
      stages: [{ stage: 1, placeholder_ms: 900, guard_max_ms: 2800 }]
    })
  })

  it('normalizes legacy scalar values exactly like the original editor', () => {
    const config = readOpenAIApiKeyFirstTokenTimeoutStageConfig({
      openai_apikey_first_token_timeout_placeholder_ms: 9999.8,
      openai_apikey_first_token_timeout_placeholder_guard_max_ms: -1
    })

    expect(config).toEqual({
      stages: [{ stage: 1, placeholder_ms: 3000, guard_max_ms: 3000 }]
    })
  })
})
