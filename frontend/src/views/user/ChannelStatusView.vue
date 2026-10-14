<template>
  <AppLayout>
    <MonitorHero
      :overall-status="overallStatus"
      :interval-seconds="DEFAULT_INTERVAL_SECONDS"
      :window="currentWindow"
      :loading="loading"
      :auto-refresh="autoRefresh"
      @update:window="handleWindowChange"
      @refresh="manualReload"
    />

    <MonitorCardGrid
      :items="items"
      :window="currentWindow"
      :countdown-seconds="countdown"
      :loading="loading"
      :detail-cache="detailCache"
      @card-click="openDetail"
    />

    <MonitorDetailDialog
      :show="showDetail"
      :monitor-id="detailTarget?.id ?? null"
      :title="detailTitle"
      @close="closeDetail"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onMounted, onBeforeUnmount, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import {
  list as listChannelMonitorViews,
  status as fetchChannelMonitorDetail,
  type UserMonitorView,
  type UserMonitorDetail,
} from '@/api/channelMonitor'
import AppLayout from '@/components/layout/AppLayout.vue'
import MonitorHero, {
  type MonitorWindow,
  type OverallStatus,
} from '@/components/user/monitor/MonitorHero.vue'
import MonitorCardGrid from '@/components/user/monitor/MonitorCardGrid.vue'
import MonitorDetailDialog from '@/components/user/MonitorDetailDialog.vue'
import { DEFAULT_INTERVAL_SECONDS, STATUS_OPERATIONAL } from '@/constants/channelMonitor'
import { useAutoRefresh } from '@/composables/useAutoRefresh'

const { t } = useI18n()
const appStore = useAppStore()

// ── State ──
const items = ref<UserMonitorView[]>([])
const loading = ref(false)
const currentWindow = ref<MonitorWindow>('7d')
const detailCache = reactive<Record<number, UserMonitorDetail>>({})
const showDetail = ref(false)
const detailTarget = ref<UserMonitorView | null>(null)

let abortController: AbortController | null = null
let detailController: AbortController | null = null
let refreshTask: Promise<void> | null = null
let disposed = false

const autoRefresh = useAutoRefresh({
  storageKey: 'channel-status-auto-refresh',
  intervals: [30, 60, 120] as const,
  defaultInterval: DEFAULT_INTERVAL_SECONDS,
  defaultEnabled: true,
  onRefresh: () => reload(true),
  shouldPause: () => document.hidden || refreshTask !== null,
})
const countdown = autoRefresh.countdown

// ── Computed ──
const overallStatus = computed<OverallStatus>(() => {
  if (items.value.length === 0) return 'operational'
  for (const it of items.value) {
    if (it.primary_status === 'failed' || it.primary_status === 'error') return 'degraded'
    if (it.primary_status !== STATUS_OPERATIONAL) return 'degraded'
  }
  return 'operational'
})

const detailTitle = computed(() => {
  return detailTarget.value?.name || t('channelStatus.detailTitle')
})

// ── Loaders ──
function reload(silent = false): Promise<void> {
  // Join the current round; repeated refreshes must not restart it indefinitely.
  if (!silent) loading.value = true
  if (refreshTask) return refreshTask
  refreshTask = reloadRound(silent).finally(() => {
    refreshTask = null
    if (!disposed) loading.value = false
  })
  return refreshTask
}

async function reloadRound(silent: boolean) {
  const ctrl = new AbortController()
  abortController = ctrl
  detailController?.abort()
  try {
    const res = await listChannelMonitorViews({ signal: ctrl.signal })
    if (disposed || ctrl.signal.aborted) return
    items.value = res.items || []
    const ids = new Set(items.value.map(it => it.id))
    for (const id of Object.keys(detailCache)) {
      if (!ids.has(Number(id))) delete detailCache[Number(id)]
    }
    await ensureDetailsForWindow(true, silent)
  } catch (err: unknown) {
    const e = err as { name?: string; code?: string }
    if (disposed || ctrl.signal.aborted || e?.name === 'AbortError' || e?.code === 'ERR_CANCELED') return
    if (!silent) appStore.showError(extractApiErrorMessage(err, t('channelStatus.loadError')))
  } finally {
    if (abortController === ctrl) {
      autoRefresh.resetCountdown()
      abortController = null
    }
  }
}

async function manualReload() {
  await reload(false)
}

async function ensureDetailsForWindow(force = false, silent = false) {
  detailController?.abort()
  if (disposed || currentWindow.value === '7d') return
  const ctrl = new AbortController()
  detailController = ctrl
  const queue = items.value.filter(it => force || !detailCache[it.id]).map(it => it.id)
  let cursor = 0
  let reported = false
  async function worker() {
    while (!disposed && !ctrl.signal.aborted && cursor < queue.length) {
      const id = queue[cursor++]
      try {
        const detail = await fetchChannelMonitorDetail(id, { signal: ctrl.signal })
        if (!disposed && !ctrl.signal.aborted) detailCache[id] = detail
      } catch (err: unknown) {
        // Retain the last good snapshot and report at most once per manual round.
        if (!disposed && !ctrl.signal.aborted && !silent && !reported) {
          reported = true
          appStore.showError(extractApiErrorMessage(err, t('channelStatus.detailLoadError')))
        }
      }
    }
  }
  await Promise.all(Array.from({ length: Math.min(4, queue.length) }, worker))
}

// ── Handlers ──
async function handleWindowChange(value: MonitorWindow) {
  currentWindow.value = value
  await ensureDetailsForWindow()
}

function openDetail(row: UserMonitorView) {
  detailTarget.value = row
  showDetail.value = true
}

function closeDetail() {
  showDetail.value = false
  detailTarget.value = null
}

watch(
  () => appStore.cachedPublicSettings?.channel_monitor_enabled,
  (enabled) => {
    if (enabled === false) autoRefresh.stop()
    else if (autoRefresh.enabled.value) autoRefresh.start()
  },
)

onMounted(() => {
  void reload(false)
  if (appStore.cachedPublicSettings?.channel_monitor_enabled !== false) {
    autoRefresh.setEnabled(autoRefresh.enabled.value)
  }
})

onBeforeUnmount(() => {
  disposed = true
  if (abortController) abortController.abort()
  detailController?.abort()
})
</script>
