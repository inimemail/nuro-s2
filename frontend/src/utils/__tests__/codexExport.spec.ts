import { describe, expect, it } from 'vitest'
import { selectDefaultCodexModel, utf8Base64 } from '../codexExport'
import { buildCcSwitchImportDeeplink } from '../ccswitchImport'

describe('Codex exports', () => {
  it.each([
    [undefined, 'gpt-6.1-sol'], [[], 'gpt-6.1-sol'], [[''], 'gpt-6.1-sol'],
    [['gpt-*'], 'gpt-6.1-sol'], [['gpt-6.1-terra*'], 'gpt-6.1-terra'],
    [['gpt-6.1-terra', 'gpt-6.1-sol'], 'gpt-6.1-sol'],
    [['custom-model'], 'custom-model'], [['custom-*'], ''],
    [['gpt-*-sol'], '']
  ])('selects only an allowed default from %s', (allowed, expected) => {
    expect(selectDefaultCodexModel(allowed as string[] | undefined)).toBe(expected)
  })
  it('encodes Unicode scripts and preserves literal secrets in embedded config', () => {
    const script = 'return "余额💰"'
    expect(new TextDecoder().decode(Uint8Array.from(atob(utf8Base64(script)), c => c.charCodeAt(0)))).toBe(script)
    const secret = 'sk-"\\\n&=秘密'
    const url = buildCcSwitchImportDeeplink({ baseUrl: 'https://example.com/路径', providerName: '测试', apiKey: secret, usageScript: script, platform: 'openai', clientType: 'claude' })
    const params = new URLSearchParams(url.split('?')[1])
    expect(params.get('apiKey')).toBe(secret)
    const decoded = new TextDecoder().decode(Uint8Array.from(atob(params.get('config')!), c => c.charCodeAt(0)))
    const config = JSON.parse(decoded).config as string
    expect(config).toContain(`experimental_bearer_token = ${JSON.stringify(secret)}`)
    expect(config).toContain('x-openai-actor-authorization')
    expect(config).not.toContain('web_search = "live"')
    expect(config).not.toContain('image_generation = true')
  })
})
