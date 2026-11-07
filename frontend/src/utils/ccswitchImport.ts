import type { GroupPlatform } from '@/types'
import { selectDefaultCodexModel, utf8Base64, CODEX_IMAGE_ACTOR_HEADERS } from './codexExport'

export const OPENAI_CC_SWITCH_CODEX_MODEL = 'gpt-6.1-sol'
export const GROK_CC_SWITCH_MODEL = 'grok-4.5'

export type CcSwitchClientType = 'claude' | 'gemini'

export interface CcSwitchImportConfig {
  app: string
  endpoint: string
  model?: string
}

export interface CcSwitchImportDeeplinkInput {
  baseUrl: string
  platform?: GroupPlatform | null
  clientType: CcSwitchClientType
  providerName: string
  apiKey: string
  usageScript: string
  allowedModels?: string[] | null
}

function withV1Endpoint(baseUrl: string): string {
  const normalized = baseUrl.replace(/\/+$/, '')
  return normalized.endsWith('/v1') ? normalized : `${normalized}/v1`
}

export function resolveCcSwitchImportConfig(
  platform: GroupPlatform | undefined | null,
  clientType: CcSwitchClientType,
  baseUrl: string
): CcSwitchImportConfig {
  switch (platform || 'anthropic') {
    case 'antigravity':
      return {
        app: clientType === 'gemini' ? 'gemini' : 'claude',
        endpoint: `${baseUrl.replace(/\/+$/, '')}/antigravity`
      }
    case 'openai':
      return {
        app: 'codex',
        endpoint: withV1Endpoint(baseUrl),
        model: OPENAI_CC_SWITCH_CODEX_MODEL
      }
    case 'gemini':
      return {
        app: 'gemini',
        endpoint: baseUrl
      }
    case 'grok':
      return {
        app: 'grokbuild',
        endpoint: withV1Endpoint(baseUrl),
        model: GROK_CC_SWITCH_MODEL
      }
    default:
      return {
        app: 'claude',
        endpoint: baseUrl
      }
  }
}

export function buildCcSwitchImportDeeplink(input: CcSwitchImportDeeplinkInput): string {
  const config = resolveCcSwitchImportConfig(input.platform, input.clientType, input.baseUrl)
  const entries: [string, string][] = [
    ['resource', 'provider'],
    ['app', config.app],
    ['name', input.providerName],
    ['homepage', input.baseUrl],
    ['endpoint', config.endpoint],
    ['apiKey', input.apiKey],
    ['configFormat', 'json'],
    ['usageEnabled', 'true'],
    ['usageScript', utf8Base64(input.usageScript)],
    ['usageAutoInterval', '30']
  ]

  if (input.platform === 'openai' && input.allowedModels) {
    config.model = selectDefaultCodexModel(input.allowedModels)
  }
  if (config.model) {
    entries.splice(2, 0, ['model', config.model])
  }

  if (config.app === 'codex') {
    const content = `model_provider = "sub2api"
${config.model ? `model = ${JSON.stringify(config.model)}` : '# Select an allowed model before sending a request.'}

[model_providers.sub2api]
name = "Sub2API"
base_url = ${JSON.stringify(config.endpoint)}
wire_api = "responses"
requires_openai_auth = false
experimental_bearer_token = ${JSON.stringify(input.apiKey)}
${CODEX_IMAGE_ACTOR_HEADERS}
`
    entries.push(['config', utf8Base64(JSON.stringify({ config: content }))])
  }
  return `ccswitch://v1/import?${new URLSearchParams(entries).toString()}`
}
