import type { GroupPlatform } from '@/types'

export type OptionalGuardLimit = number | string | null | undefined

const upstreamBillingGuardPlatforms = new Set<GroupPlatform>([
  'openai',
  'anthropic',
  'gemini',
  'grok',
  'antigravity'
])

export function isUpstreamBillingGuardPlatform(platform: GroupPlatform): boolean {
  return upstreamBillingGuardPlatforms.has(platform)
}

export function normalizeUpstreamBillingGuardLimit(
  value: OptionalGuardLimit
): number | null | undefined {
  if (value === null || value === undefined) return null
  if (typeof value === 'string' && value.trim() === '') return null
  const parsed = Number(value)
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : undefined
}

export function buildUpstreamBillingGuardLimitPayload(
  platform: GroupPlatform,
  value: OptionalGuardLimit
): number | null | undefined {
  if (!isUpstreamBillingGuardPlatform(platform)) return null
  return normalizeUpstreamBillingGuardLimit(value)
}
