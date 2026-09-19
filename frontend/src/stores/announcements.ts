import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { announcementsAPI } from '@/api'
import type { UserAnnouncement } from '@/types'

const THROTTLE_MS = 20 * 60 * 1000 // 20 minutes
const FAILURE_RETRY_MS = 30 * 1000 // Avoid retrying on every route change when the endpoint is unhealthy.

export const useAnnouncementStore = defineStore('announcements', () => {
  // State
  const announcements = ref<UserAnnouncement[]>([])
  const loading = ref(false)
  const lastFetchTime = ref(0)
  const popupQueue = ref<UserAnnouncement[]>([])
  const currentPopup = ref<UserAnnouncement | null>(null)

  // Session-scoped dedup set — not reactive, used as plain lookup only
  let shownPopupIds = new Set<number>()
  let session = 0
  let fetchGeneration = 0
  let pending = 0
  let popupTimer: ReturnType<typeof setTimeout> | undefined

  // Getters
  const unreadCount = computed(() =>
    announcements.value.filter((a) => !a.read_at).length
  )

  // Actions
  async function fetchAnnouncements(force = false) {
    const now = Date.now()
    if (!force && lastFetchTime.value > 0 && now - lastFetchTime.value < THROTTLE_MS) {
      return
    }

    const currentSession = session
    const generation = ++fetchGeneration
    // Set immediately to prevent concurrent duplicate requests
    lastFetchTime.value = now

    try {
      pending++
      loading.value = true
      const all = await announcementsAPI.list(false)
      if (currentSession !== session || generation !== fetchGeneration) return
      announcements.value = all.slice(0, 20)
      enqueueNewPopups()
    } catch (err: any) {
      if (currentSession !== session || generation !== fetchGeneration) return
      // Keep a short retry window so route changes do not repeatedly hit a slow/failing endpoint.
      lastFetchTime.value = Date.now() - THROTTLE_MS + FAILURE_RETRY_MS
      console.error('Failed to fetch announcements:', err)
    } finally {
      if (currentSession === session) loading.value = --pending > 0
    }
  }

  function enqueueNewPopups() {
    const newPopups = announcements.value.filter(
      (a) => a.notify_mode === 'popup' && !a.read_at && !shownPopupIds.has(a.id)
    )
    if (newPopups.length === 0) return

    for (const p of newPopups) {
      if (!popupQueue.value.some((q) => q.id === p.id)) {
        popupQueue.value.push(p)
      }
    }

    if (!currentPopup.value) {
      showNextPopup()
    }
  }

  function showNextPopup() {
    clearTimeout(popupTimer)
    popupTimer = undefined
    if (popupQueue.value.length === 0) {
      currentPopup.value = null
      return
    }
    currentPopup.value = popupQueue.value.shift()!
    shownPopupIds.add(currentPopup.value.id)
  }

  async function dismissPopup() {
    if (!currentPopup.value) return
    const id = currentPopup.value.id
    currentPopup.value = null

    // Mark as read (fire-and-forget, UI already updated)
    markAsRead(id)

    // Show next popup after a short delay
    if (popupQueue.value.length > 0) {
      clearTimeout(popupTimer)
      popupTimer = setTimeout(() => showNextPopup(), 300)
    }
  }

  async function markAsRead(id: number) {
    const currentSession = session
    try {
      await announcementsAPI.markRead(id)
      if (currentSession !== session) return
      const ann = announcements.value.find((a) => a.id === id)
      if (ann) {
        ann.read_at = new Date().toISOString()
      }
    } catch (err: any) {
      console.error('Failed to mark announcement as read:', err)
    }
  }

  async function markAllAsRead() {
    const currentSession = session
    const unread = announcements.value.filter((a) => !a.read_at)
    if (unread.length === 0) return

    try {
      pending++
      loading.value = true
      const results = await Promise.allSettled(unread.map(async (a) => {
        await announcementsAPI.markRead(a.id)
        if (currentSession !== session) return
        a.read_at = new Date().toISOString()
        const current = announcements.value.find((item) => item.id === a.id)
        if (current) current.read_at = a.read_at
      }))
      const failure = results.find((result) => result.status === 'rejected')
      if (failure) throw failure.reason
    } catch (err: any) {
      console.error('Failed to mark all as read:', err)
      throw err
    } finally {
      if (currentSession === session) loading.value = --pending > 0
    }
  }

  function reset() {
    session++
    fetchGeneration++
    pending = 0
    clearTimeout(popupTimer)
    announcements.value = []
    lastFetchTime.value = 0
    shownPopupIds = new Set()
    popupQueue.value = []
    currentPopup.value = null
    loading.value = false
  }

  return {
    // State
    announcements,
    loading,
    currentPopup,
    // Getters
    unreadCount,
    // Actions
    fetchAnnouncements,
    dismissPopup,
    markAsRead,
    markAllAsRead,
    reset,
  }
})
