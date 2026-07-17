import type { GroupPlatform } from '@/types'

export type OptionalGuardLimit = number | string | null | undefined

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
  if (platform !== 'openai') return null
  return normalizeUpstreamBillingGuardLimit(value)
}
