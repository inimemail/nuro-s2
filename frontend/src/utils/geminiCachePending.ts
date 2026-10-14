export interface PendingCacheOperation {
  keyID: number
  id: string
  kind: 'create' | 'patch' | 'delete'
  createdAt: number
}
export function pendingCacheStorageKey(userID: number) { return `nuro:gemini-cache:pending:${userID}` }
export function readPendingCacheOperation(userID: number): PendingCacheOperation | null {
  try {
    const value = JSON.parse(localStorage.getItem(pendingCacheStorageKey(userID)) || 'null') as PendingCacheOperation | null
    if (!value || !Number.isSafeInteger(value.keyID) || value.keyID <= 0 || !/^[a-f0-9]{32}$/.test(value.id) || !['create', 'patch', 'delete'].includes(value.kind) || !Number.isFinite(value.createdAt) || value.createdAt <= 0) return null
    return value
  } catch { return null }
}
export function savePendingCacheOperation(userID: number, value: PendingCacheOperation) {
  localStorage.setItem(pendingCacheStorageKey(userID), JSON.stringify(value))
}
export function clearPendingCacheOperation(userID: number, id: string) {
  if (readPendingCacheOperation(userID)?.id === id) localStorage.removeItem(pendingCacheStorageKey(userID))
}
