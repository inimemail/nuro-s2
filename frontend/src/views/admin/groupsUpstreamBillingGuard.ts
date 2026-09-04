import type { GroupPlatform } from '@/types'

export type OptionalGuardLimit = number | string | null | undefined

const upstreamBillingGuardPlatforms = new Set<GroupPlatform>([
  'openai',
  'anthropic',
  'gemini',
  'grok',
  'antigravity',
  'kimi',
  'zhipu',
  'deepseek'
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

export function buildUpstreamBillingGuardBoundsPayload(
  platform: GroupPlatform,
  minValue: OptionalGuardLimit,
  maxValue: OptionalGuardLimit
): { min: number | null | undefined; max: number | null | undefined; valid: boolean } {
  if (!isUpstreamBillingGuardPlatform(platform)) return { min: null, max: null, valid: true }
  const min = normalizeUpstreamBillingGuardLimit(minValue)
  const max = normalizeUpstreamBillingGuardLimit(maxValue)
  const valid = min !== undefined && max !== undefined && (min === null || max === null || min < max)
  return { min, max, valid }
}
