import { describe, expect, it } from 'vitest'
import { supportsPricingModelSync } from '../channelModelSync'
import { defaultOpenCodeRules } from '../opencode'

describe('local platform capabilities', () => {
  it('offers pricing sync only for providers supported by the local endpoint', () => {
    for (const platform of ['anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax']) {
      expect(supportsPricingModelSync(platform)).toBe(true)
    }
    for (const platform of ['opencode_go', 'typesafe', 'composite', 'command_code', 'cline', 'unknown']) {
      expect(supportsPricingModelSync(platform)).toBe(false)
    }
  })

  it('keeps Zen qwen3.8-max on Chat and Go qwen on Anthropic', () => {
    expect(defaultOpenCodeRules('zen')[0]).toEqual({ pattern: 'qwen3.8-max', protocol: 'chat_completions' })
    expect(defaultOpenCodeRules('go').find(rule => rule.pattern === 'qwen*')?.protocol).toBe('anthropic')
    expect(defaultOpenCodeRules('go').some(rule => rule.pattern === 'qwen3.8-max')).toBe(false)
  })
})
