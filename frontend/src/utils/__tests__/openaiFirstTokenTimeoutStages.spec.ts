import { describe, expect, it } from 'vitest'
import {
  readOpenAIApiKeyFirstTokenTimeoutStageConfig,
  validateOpenAIApiKeyFirstTokenTimeoutStageConfig
} from '../openaiFirstTokenTimeoutStages'

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

  it('normalizes legacy scalar values into a valid stage', () => {
    const config = readOpenAIApiKeyFirstTokenTimeoutStageConfig({
      openai_apikey_first_token_timeout_placeholder_ms: 9999.8,
      openai_apikey_first_token_timeout_placeholder_guard_max_ms: -1
    })

    expect(config).toEqual({
      stages: [{ stage: 1, placeholder_ms: 9999, guard_max_ms: 9999 }]
    })
  })

  it('accepts the API key placeholder maximum and an uncapped guard', () => {
    const error = validateOpenAIApiKeyFirstTokenTimeoutStageConfig({
      stages: [{ stage: 1, placeholder_ms: 100000, guard_max_ms: 900000 }]
    }, (key) => key)

    expect(error).toBeNull()
  })

  it('raises a legacy scalar guard to a larger placeholder', () => {
    const config = readOpenAIApiKeyFirstTokenTimeoutStageConfig({
      openai_apikey_first_token_timeout_placeholder_ms: 50000,
      openai_apikey_first_token_timeout_placeholder_guard_max_ms: 3000
    })

    expect(config.stages[0]).toEqual({ stage: 1, placeholder_ms: 50000, guard_max_ms: 50000 })
  })

  it('rejects an API key placeholder above 100000ms', () => {
    const error = validateOpenAIApiKeyFirstTokenTimeoutStageConfig({
      stages: [{ stage: 1, placeholder_ms: 100001, guard_max_ms: 900000 }]
    }, (key) => key)

    expect(error).toContain('stagePlaceholderInvalid')
  })

  it('rejects a newly added blank stage before save', () => {
    const error = validateOpenAIApiKeyFirstTokenTimeoutStageConfig({
      stages: [
        { stage: 1, placeholder_ms: 800, guard_max_ms: 5000 },
        { stage: 2, placeholder_ms: null, guard_max_ms: null }
      ]
    }, (key) => key)

    expect(error).toContain('stagePlaceholderInvalid')
  })
})
