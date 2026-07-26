<template>
  <section v-if="eligible" class="border-t border-gray-200 pt-4 dark:border-dark-700">
    <div class="flex items-start justify-between gap-4">
      <div class="flex min-w-0 items-start gap-2.5">
        <span class="mt-0.5 rounded-md bg-emerald-100 p-1.5 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300">
          <Icon name="cloud" size="sm" />
        </span>
        <div class="min-w-0">
          <h3 class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('admin.accounts.ollamaUsageTitle') }}</h3>
          <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.ollamaUsageHint') }}</p>
        </div>
      </div>
      <span v-if="state" :class="statusBadgeClass" class="shrink-0 rounded-full px-2 py-0.5 text-[11px] font-medium">
        {{ statusLabel }}
      </span>
    </div>

    <div class="mt-3 space-y-3">
      <div class="flex flex-col gap-2 sm:flex-row">
        <input
          v-model="session"
          type="password"
          autocomplete="new-password"
          class="input min-w-0 flex-1"
          :placeholder="t('admin.accounts.ollamaSessionPlaceholder')"
          :aria-label="t('admin.accounts.ollamaSession')"
          @keyup.enter="save"
        />
        <button type="button" class="btn btn-secondary w-full shrink-0 sm:w-auto" :disabled="busy || !session.trim()" @click="save">
          <Icon name="key" size="sm" class="mr-1.5" />
          {{ t('admin.accounts.ollamaSaveSession') }}
        </button>
      </div>

      <div v-if="state?.configured" class="flex flex-col gap-3 sm:flex-row sm:flex-wrap sm:items-center sm:justify-between">
        <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
          <Toggle :model-value="state.auto_refresh_enabled" :disabled="busy" @update:model-value="toggle" />
          <span>{{ t('admin.accounts.ollamaAccountAutoRefresh') }}</span>
        </label>
        <div class="flex flex-col gap-2 sm:flex-row sm:items-center">
          <button type="button" class="btn btn-secondary w-full sm:w-auto" :disabled="busy" @click="refresh">
            <Icon name="refresh" size="sm" class="mr-1.5" :class="{ 'animate-spin': busyAction === 'refresh' }" />
            {{ t('admin.accounts.ollamaRefreshNow') }}
          </button>
          <button type="button" class="btn btn-secondary w-full text-red-600 hover:text-red-700 dark:text-red-400 sm:w-auto" :disabled="busy" @click="remove">
            <Icon name="trash" size="sm" class="mr-1.5" />
            {{ t('admin.accounts.ollamaRemoveSession') }}
          </button>
        </div>
      </div>

      <div v-if="state?.snapshot?.data" class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <UsageProgress :label="t('admin.accounts.ollamaFiveHour')" :window="state.snapshot.data.five_hour" />
        <UsageProgress :label="t('admin.accounts.ollamaSevenDay')" :window="state.snapshot.data.seven_day" />
      </div>

      <div v-if="state?.snapshot" class="flex flex-wrap gap-x-5 gap-y-1 text-xs text-gray-500 dark:text-gray-400">
        <span>{{ t('admin.accounts.ollamaLastRefresh') }}: {{ formatTime(state.snapshot.fetched_at || state.snapshot.last_attempt_at) }}</span>
        <span>{{ t('admin.accounts.ollamaNextRefresh') }}: {{ formatTime(state.snapshot.next_refresh_at) }}</span>
      </div>

      <div v-if="globalSettings" class="border-t border-gray-200 pt-3 dark:border-dark-700">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div>
            <p class="text-sm font-medium text-gray-800 dark:text-gray-200">{{ t('admin.accounts.ollamaBackgroundRefresh') }}</p>
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.ollamaBackgroundRefreshHint') }}</p>
          </div>
          <Toggle v-model="globalSettings.enabled" :disabled="busy" @update:model-value="saveGlobal" />
        </div>
        <div class="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
          <label class="block">
            <span class="input-label">{{ t('admin.accounts.ollamaMaxWaitMinutes') }}</span>
            <input v-model.number="globalSettings.interval_minutes" type="number" min="15" max="1440" class="input" :disabled="busy" @change="saveGlobal" />
          </label>
          <label class="block">
            <span class="input-label">{{ t('admin.accounts.ollamaDebounceMinutes') }}</span>
            <input v-model.number="globalSettings.debounce_minutes" type="number" min="1" max="60" class="input" :disabled="busy" @change="saveGlobal" />
          </label>
        </div>
      </div>

      <p v-if="state && !state.encryption_key_configured" class="flex items-center gap-1.5 text-xs text-amber-700 dark:text-amber-300">
        <Icon name="exclamationTriangle" size="sm" />
        {{ t('admin.accounts.ollamaEncryptionRequired') }}
      </p>
      <p v-if="error" class="flex items-center gap-1.5 text-xs text-red-600 dark:text-red-400">
        <Icon name="exclamationCircle" size="sm" />
        {{ error }}
      </p>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, defineComponent, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import Icon from '@/components/icons/Icon.vue'
import Toggle from '@/components/common/Toggle.vue'
import type { Account, OllamaCloudUsageState, OllamaCloudUsageSettings as GlobalSettings, OllamaCloudUsageWindow } from '@/types'

const props = defineProps<{ account: Account }>()
const { t, locale } = useI18n()
const state = ref<OllamaCloudUsageState | null>(null)
const globalSettings = ref<GlobalSettings | null>(null)
const session = ref('')
const busyAction = ref('')
const error = ref('')
const busy = computed(() => busyAction.value !== '')

const eligible = computed(() => {
  const baseURL = String((props.account.credentials as Record<string, unknown> | undefined)?.base_url || '').replace(/\/$/, '').toLowerCase()
  return props.account.type === 'apikey' && ['openai', 'anthropic'].includes(props.account.platform) && ['https://ollama.com', 'https://ollama.com/v1'].includes(baseURL)
})

const statusLabel = computed(() => {
  if (!state.value?.configured) return t('admin.accounts.ollamaNotConfigured')
  switch (state.value.snapshot?.status) {
    case 'ok': return t('admin.accounts.ollamaStatusReady')
    case 'unauthorized': return t('admin.accounts.ollamaStatusUnauthorized')
    case 'failed': return t('admin.accounts.ollamaStatusFailed')
    default: return t('admin.accounts.ollamaConfigured')
  }
})

const statusBadgeClass = computed(() => {
  if (state.value?.snapshot?.status === 'ok') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
  if (state.value?.snapshot?.status === 'failed' || state.value?.snapshot?.status === 'unauthorized') return 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
  return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
})

const UsageProgress = defineComponent({
  props: { label: { type: String, required: true }, window: { type: Object as () => OllamaCloudUsageWindow | undefined, default: undefined } },
  setup(componentProps) {
    return () => {
      const percent = Math.max(0, Math.min(100, componentProps.window?.used_percent ?? 0))
      return h('div', { class: 'space-y-1' }, [
        h('div', { class: 'flex items-center justify-between text-xs' }, [
          h('span', { class: 'font-medium text-gray-700 dark:text-gray-300' }, componentProps.label),
          h('span', { class: 'tabular-nums text-gray-500 dark:text-gray-400' }, componentProps.window ? `${Math.round(percent)}%` : '--')
        ]),
        h('div', { class: 'h-1.5 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-700' }, [
          h('div', { class: percent >= 90 ? 'h-full bg-red-500' : percent >= 70 ? 'h-full bg-amber-500' : 'h-full bg-emerald-500', style: { width: `${percent}%` } })
        ])
      ])
    }
  }
})

function errorMessage(cause: unknown): string {
  const candidate = cause as { response?: { data?: { error?: string; message?: string } }; message?: string }
  return candidate?.response?.data?.message || candidate?.response?.data?.error || candidate?.message || t('admin.accounts.ollamaRequestFailed')
}

async function run(action: string, operation: () => Promise<OllamaCloudUsageState>) {
  busyAction.value = action
  error.value = ''
  try { state.value = await operation() } catch (cause) { error.value = errorMessage(cause) } finally { busyAction.value = '' }
}

async function load() {
  await run('load', () => adminAPI.accounts.getOllamaCloudUsage(props.account.id))
  try { globalSettings.value = await adminAPI.accounts.getOllamaCloudUsageSettings() } catch (cause) { error.value = errorMessage(cause) }
}
async function save() { if (session.value.trim()) await run('save', async () => { const result = await adminAPI.accounts.saveOllamaCloudUsageSession(props.account.id, session.value); session.value = ''; return result }) }
async function refresh() { await run('refresh', () => adminAPI.accounts.refreshOllamaCloudUsage(props.account.id)) }
async function remove() { await run('remove', () => adminAPI.accounts.deleteOllamaCloudUsageSession(props.account.id)) }
async function toggle(enabled: boolean) { await run('toggle', () => adminAPI.accounts.setOllamaCloudUsageAutoRefresh(props.account.id, enabled)) }
async function saveGlobal() {
  if (!globalSettings.value) return
  busyAction.value = 'settings'; error.value = ''
  try { globalSettings.value = await adminAPI.accounts.updateOllamaCloudUsageSettings(globalSettings.value) } catch (cause) { error.value = errorMessage(cause) } finally { busyAction.value = '' }
}
function formatTime(value?: string): string { return value ? new Intl.DateTimeFormat(locale.value, { dateStyle: 'short', timeStyle: 'short' }).format(new Date(value)) : '--' }

onMounted(() => { if (eligible.value) void load() })
</script>
