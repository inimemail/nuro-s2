import type { Account } from '@/types'

export function isTruthyCredentialFlag(value: unknown): boolean {
  if (value === true) return true
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase()
    return normalized === 'true' || normalized === '1' || normalized === 'yes' || normalized === 'y' || normalized === 'on'
  }
  if (typeof value === 'number') return value !== 0
  return false
}

export function isPoolModeAccount(account: Account | null | undefined): boolean {
  if (!account || (account.type !== 'apikey' && account.type !== 'bedrock')) return false
  return isTruthyCredentialFlag(account.credentials?.pool_mode)
}

export function isImagePoolModeAccount(account: Account | null | undefined): boolean {
  return isPoolModeAccount(account) && isTruthyCredentialFlag(account?.credentials?.image_pool_mode)
}
