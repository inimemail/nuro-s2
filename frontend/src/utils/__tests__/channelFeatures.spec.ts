import { describe, expect, it } from 'vitest'
import { channelBedrockCompatibility, channelFeatureBoolean } from '../channelFeatures'

describe('channel Bedrock feature payload', () => {
  it('normalizes legacy booleans without treating string false as truthy', () => {
    for (const value of [true, 1, '1', 'true']) expect(channelFeatureBoolean(value)).toBe(true)
    for (const value of [false, 0, '0', 'false', undefined, null]) expect(channelFeatureBoolean(value)).toBe(false)
  })
  it('writes an explicit boolean and preserves unrelated features', () => {
    const original = { bedrock_cc_compat: true, custom: { keep: 1 } }
    expect(channelBedrockCompatibility(original, [{ enabled: true, platform: 'anthropic', bedrock_cc_compat: false }]))
      .toEqual({ bedrock_cc_compat: false, custom: { keep: 1 } })
    expect(original.bedrock_cc_compat).toBe(true)
    expect(channelBedrockCompatibility(original, [{ enabled: true, platform: 'openai', bedrock_cc_compat: false }])).toEqual(original)
    expect(channelBedrockCompatibility({}, [])).toEqual({})
  })
})
