const activeBodyScrollLocks = new Set<symbol>()
let previousBodyOverflow = ''

export function acquireBodyScrollLock(owner: symbol): void {
  if (typeof document === 'undefined' || activeBodyScrollLocks.has(owner)) return
  if (activeBodyScrollLocks.size === 0) previousBodyOverflow = document.body.style.overflow
  activeBodyScrollLocks.add(owner)
  document.body.style.overflow = 'hidden'
}

export function releaseBodyScrollLock(owner: symbol): void {
  if (typeof document === 'undefined' || !activeBodyScrollLocks.delete(owner)) return
  if (activeBodyScrollLocks.size === 0) {
    document.body.style.overflow = previousBodyOverflow
    previousBodyOverflow = ''
  }
}
