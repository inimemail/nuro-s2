import { describe, expect, it } from 'vitest'
import en from '../locales/en'
import zh from '../locales/zh'

const gatewayForwardingKeys = [
  'codexCLIOnlyPolicy',
  'codexCLIOnlyPolicyHint',
  'codexCLIOnlyBlacklist',
  'codexCLIOnlyBlacklistPlaceholder',
  'codexCLIOnlyBlacklistHint',
  'codexCLIOnlyWhitelist',
  'codexCLIOnlyWhitelistPlaceholder',
  'codexCLIOnlyWhitelistHint',
  'codexCLIOnlyEngineFingerprintSignals',
  'codexCLIOnlyEngineFingerprintSignalsPlaceholder',
  'codexCLIOnlyEngineFingerprintSignalsHint'
] as const

describe('gateway forwarding locale messages', () => {
  it('avoid raw JSON braces in translated settings copy', () => {
    for (const locale of ['zh', 'en'] as const) {
      const messages = locale === 'zh' ? zh : en
      for (const key of gatewayForwardingKeys) {
        const message = messages.admin.settings.gatewayForwarding[key]
        expect(message).toBeTruthy()
        expect(message).not.toMatch(/[{}]/)
      }
    }
  })
})
