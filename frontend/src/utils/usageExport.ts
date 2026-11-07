// Bounded retry helpers shared by CSV exports. No request is replayed except
// a read-only page that returned 429; cursor advancement belongs to the caller.
export function usageExportRetryDelay(raw: unknown, retry: number, now = Date.now()): number {
  const text = typeof raw === 'string' ? raw.trim() : ''
  let ms = /^\d+(?:\.\d+)?$/.test(text) ? Number(text) * 1000 : Date.parse(text) - now
  if (!Number.isFinite(ms) || ms < 0) ms = Math.min(1000 * 2 ** retry, 10000)
  return Math.min(Math.max(ms, 250), 60000)
}

export function waitForUsageExport(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(new DOMException('Cancelled', 'AbortError')); return }
    const finish = () => { signal.removeEventListener('abort', abort); resolve() }
    const timer = setTimeout(finish, ms)
    const abort = () => {
      clearTimeout(timer)
      signal.removeEventListener('abort', abort)
      reject(new DOMException('Cancelled', 'AbortError'))
    }
    signal.addEventListener('abort', abort, { once: true })
  })
}
