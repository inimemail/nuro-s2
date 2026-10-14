import axios from 'axios'
import { apiClient as client } from './client'

export interface GeminiCacheBilling {
  reservedAmount: string
  settledAmount: string
  inputCost: string
  storageCost: string
  operationState: string
}
export interface GeminiCachedContent {
  name: string
  displayName: string
  model: string
  expireTime: string
  createTime: string
  updateTime: string
  state: string
  usageMetadata: { totalTokenCount: number }
  billing: GeminiCacheBilling
}
export interface GeminiCacheQuote {
  tokens: number
  tokenSource: string
  inputCost: string
  storageCost: string
  amount: string
  paidUntil: string
  currency: string
}
export interface GeminiCacheCreate {
  model: string
  displayName?: string
  contents: Array<{ role: string; parts: Array<{ text: string }> }>
  ttl: string
}

export interface GeminiCacheKey { id: number; name: string; group: { name: string } }
function config(signal?: AbortSignal, idempotency?: string) {
  return { signal, timeout: 110000, headers: { ...(idempotency ? { 'Idempotency-Key': idempotency } : {}) } }
}
function collectionURL(keyID: number) {
  if (!Number.isSafeInteger(keyID) || keyID <= 0) throw new Error('Invalid key ID')
  return `/user/gemini-cache/${keyID}/cachedContents`
}
function resourceURL(keyID: number, name: string) {
  if (!/^cachedContents\/nuro_[a-f0-9]{32}$/.test(name)) throw new Error('Invalid cached content name')
  return `${collectionURL(keyID)}/${name.slice('cachedContents/'.length)}`
}
export async function listCacheKeys(page = 1, search = '', signal?: AbortSignal) {
  const { data } = await client.get<{ items: GeminiCacheKey[]; pages: number; page: number }>('/user/gemini-cache/keys', { signal, params: { page, search } })
  return data
}
export async function listCaches(keyID: number, pageToken = '', signal?: AbortSignal) {
  const { data } = await client.get<{ cachedContents: GeminiCachedContent[]; nextPageToken?: string }>(collectionURL(keyID), {
    ...config(signal), params: { pageSize: 50, pageToken }
  })
  return data
}
export async function quoteCache(keyID: number, body: GeminiCacheCreate, signal?: AbortSignal) {
  const { data } = await client.post<GeminiCacheQuote>(collectionURL(keyID), body, {
    ...config(signal), params: { dryRun: true }
  })
  return data
}
export async function createCache(keyID: number, body: GeminiCacheCreate, idempotency: string, signal?: AbortSignal) {
  const { data } = await client.post<GeminiCachedContent>(collectionURL(keyID), body, config(signal, idempotency))
  return data
}
export async function renewCache(keyID: number, name: string, expiry: string, idempotency: string, signal?: AbortSignal) {
  const { data } = await client.patch<GeminiCachedContent>(resourceURL(keyID, name), { expireTime: expiry }, {
    ...config(signal, idempotency), params: { updateMask: 'expireTime' }
  })
  return data
}
export async function quoteRenewCache(keyID: number, name: string, expiry: string, signal?: AbortSignal) {
  const { data } = await client.patch<GeminiCacheQuote>(resourceURL(keyID, name), { expireTime: expiry }, { ...config(signal), params: { updateMask: 'expireTime', dryRun: true } })
  return data
}
export async function deleteCache(keyID: number, name: string, idempotency: string, signal?: AbortSignal) {
  await client.delete(resourceURL(keyID, name), config(signal, idempotency))
}
export async function cacheOperationStatus(keyID: number, idempotency: string, kind: string, signal?: AbortSignal) {
  const { data } = await client.get<{ state: string; resourceName: string }>(`/user/gemini-cache/${keyID}/operations/${encodeURIComponent(idempotency)}`, { ...config(signal), params: { kind } })
  return data
}
export async function cancelUnrecordedCacheOperation(keyID: number, idempotency: string, kind: string) {
  await client.post(`/user/gemini-cache/${keyID}/operations/${encodeURIComponent(idempotency)}/cancel-unrecorded`, {}, { params: { kind } })
}
export function cacheErrorKind(error: unknown, mutation = false): 'pending' | 'quota' | 'unavailable' | 'missing' | 'failed' {
  const status = axios.isAxiosError(error) ? error.response?.status : (error as { status?: number })?.status
  if (status === 409 || (mutation && (!status || status >= 500))) return 'pending'
  if (status === 429) return 'quota'
  if (status === 503) return 'unavailable'
  if (status === 404) return 'missing'
  return 'failed'
}
