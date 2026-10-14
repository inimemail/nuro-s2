import { onUnmounted, ref } from 'vue'

/** Intervals only repaint; elapsed time is always derived from the order deadline. */
export function useDeadlineCountdown(onExpired: () => void, fallbackSeconds = 30 * 60) {
  const remainingSeconds = ref(0)
  const fallbackDeadlines = new Map<string | number, number>()
  let deadline = 0
  let timer: ReturnType<typeof setInterval> | undefined
  let active = false

  function stop() {
    active = false
    clearInterval(timer)
    timer = undefined
  }

  function refresh() {
    if (!active) return
    remainingSeconds.value = Math.max(0, Math.ceil((deadline - Date.now()) / 1000))
    if (remainingSeconds.value === 0) {
      stop()
      onExpired()
    }
  }

  function start(expiresAt: string | undefined, orderId: string | number) {
    stop()
    if (expiresAt) {
      const parsed = Date.parse(expiresAt)
      deadline = Number.isFinite(parsed) ? parsed : 0
    } else {
      // Reopening the same order must not grant it a fresh fallback lifetime.
      deadline = fallbackDeadlines.get(orderId) ?? Date.now() + fallbackSeconds * 1000
      fallbackDeadlines.set(orderId, deadline)
      if (fallbackDeadlines.size > 8) fallbackDeadlines.delete(fallbackDeadlines.keys().next().value!)
    }
    active = true
    refresh()
    if (active) timer = setInterval(refresh, 1000)
  }

  function onVisibilityChange() {
    if (!document.hidden) refresh()
  }
  document.addEventListener('visibilitychange', onVisibilityChange)
  onUnmounted(() => {
    stop()
    document.removeEventListener('visibilitychange', onVisibilityChange)
  })

  return { remainingSeconds, start, stop }
}
