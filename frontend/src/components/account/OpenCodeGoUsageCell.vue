<template>
  <div v-if="eligible" class="w-48 max-w-full rounded-lg border border-gray-200/80 bg-gray-50/70 p-2.5 dark:border-dark-600 dark:bg-dark-800/50">
    <div class="mb-2 flex items-center justify-between gap-2">
      <span class="text-[11px] font-semibold text-gray-700 dark:text-gray-200">OpenCode <span class="text-primary-600 dark:text-primary-400">Go</span></span>
      <button type="button" class="rounded-md p-1 text-gray-500 transition hover:bg-white hover:text-primary-600 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 disabled:opacity-40 dark:hover:bg-dark-700" :disabled="busy" :title="t('openCodeUsage.refresh')" :aria-label="t('openCodeUsage.refresh')" @click="run('refresh')">
        <Icon name="refresh" size="xs" :class="{ 'animate-spin': action === 'refresh' }" />
      </button>
    </div>
    <div class="space-y-2">
      <div v-for="window in windows" :key="window.key" :title="resetLabel(window.tier?.reset_at)">
        <div class="mb-1 flex items-center justify-between text-[10px]">
          <span class="text-gray-500 dark:text-gray-400">{{ t(`openCodeUsage.${window.key}`) }}</span>
          <span class="font-medium tabular-nums text-gray-700 dark:text-gray-200">{{ window.tier ? `${Math.round(window.tier.used_percent)}%` : '—' }}</span>
        </div>
        <div class="h-1.5 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600" role="progressbar" :aria-label="t(`openCodeUsage.${window.key}`)" :aria-valuenow="window.tier ? Math.min(100, window.tier.used_percent) : undefined" :aria-valuemin="0" :aria-valuemax="100">
          <div class="h-full rounded-full transition-all duration-300" :class="(window.tier?.used_percent ?? 0) >= 90 ? 'bg-rose-500' : (window.tier?.used_percent ?? 0) >= 70 ? 'bg-amber-500' : 'bg-primary-500'" :style="{ width: `${Math.min(100, window.tier?.used_percent ?? 0)}%` }" />
        </div>
      </div>
    </div>
    <p class="mt-2 text-[9px] text-gray-400" :title="snapshot?.fetched_at ? formatTime(snapshot.fetched_at) : ''">{{ snapshot?.fetched_at ? t('openCodeUsage.updated', { time: formatTime(snapshot.fetched_at) }) : t('openCodeUsage.notFetched') }}</p>
    <p v-if="error || snapshot?.error" class="mt-2 break-words rounded-md bg-amber-50 px-2 py-1.5 text-[10px] leading-relaxed text-amber-700 dark:bg-amber-900/20 dark:text-amber-300" role="status">{{ error || snapshot?.error }}</p>
    <details class="mt-2 border-t border-gray-200 pt-2 text-[10px] dark:border-dark-600">
      <summary class="cursor-pointer text-gray-500 hover:text-primary-600 dark:text-gray-400">{{ t('openCodeUsage.preferences') }}<span v-if="state?.auto_refresh" class="ml-1 text-primary-600">· {{ t('openCodeUsage.auto') }}</span></summary>
      <div class="mt-2 space-y-2">
        <label class="flex items-center gap-2 text-gray-700 dark:text-gray-300"><input v-model="autoRefresh" type="checkbox" :disabled="busy || !state" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />{{ t('openCodeUsage.accountAuto') }}</label>
        <label v-if="account.platform === 'opencode_go'" class="block text-gray-500">
          {{ t('openCodeUsage.source') }}
          <select v-model="source" class="input mt-1 w-full !px-2 !py-1 text-[11px]" :disabled="busy || !state">
            <option value="configured">{{ t('openCodeUsage.configured') }}</option>
            <option value="official">{{ t('openCodeUsage.official') }}</option>
          </select>
        </label>
        <p class="leading-relaxed text-gray-400">{{ source === 'official' ? t('openCodeUsage.officialHint') : t('openCodeUsage.configuredHint') }}</p>
        <p v-if="state && !state.global_enabled" class="text-amber-600 dark:text-amber-400">{{ t('openCodeUsage.globalOff') }}</p>
        <button type="button" class="w-full rounded-lg bg-primary-50 px-2 py-1.5 font-medium text-primary-700 hover:bg-primary-100 disabled:opacity-40 dark:bg-primary-900/30 dark:text-primary-300" :disabled="busy || !state" @click="run('save')">{{ t('common.save') }}</button>
      </div>
    </details>
  </div>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { extractApiErrorMessage } from '@/utils/apiError'
import { isOpenCodeUsageAccount, openCodeUsageAPI, type OpenCodeUsageState } from '@/api/admin/opencodeUsage'
import type { Account } from '@/types'
const props = defineProps<{ account: Account }>()
const { t, locale } = useI18n()
const eligible = computed(() => isOpenCodeUsageAccount(props.account))
const state = ref<OpenCodeUsageState | null>(null)
const snapshot = computed(() => state.value?.snapshot)
const windows = computed(() => ['fiveHour', 'weekly', 'monthly'].map((key, i) => ({ key, tier: snapshot.value?.tiers?.find(tier => tier.window === ['5h', 'weekly', 'monthly'][i]) })))
const action = ref('')
const busy = computed(() => action.value !== '')
const error = ref('')
const autoRefresh = ref(false)
const source = ref<OpenCodeUsageState['source']>('configured')
let generation = 0
function formatTime(seconds: number) { return new Date(seconds * 1000).toLocaleString(locale.value === 'zh' ? 'zh-CN' : 'en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) }
function resetLabel(value?: string) { return value && Number.isFinite(Date.parse(value)) ? t('openCodeUsage.resets', { time: formatTime(Date.parse(value) / 1000) }) : t('openCodeUsage.unknownReset') }
async function run(kind: 'load' | 'refresh' | 'save') {
  if (!eligible.value) return
  const id = props.account.id
  const version = ++generation
  action.value = kind
  error.value = ''
  try {
    const result = await (kind === 'load' ? openCodeUsageAPI.get(id) : kind === 'refresh' ? openCodeUsageAPI.refresh(id) : openCodeUsageAPI.configure(id, autoRefresh.value, source.value))
    if (version !== generation || id !== props.account.id) return
    state.value = result
    autoRefresh.value = result.auto_refresh
    source.value = result.source
  } catch (err) {
    if (version === generation && id === props.account.id) error.value = extractApiErrorMessage(err, t('openCodeUsage.failed'))
  } finally { if (version === generation) action.value = '' }
}
watch(() => [props.account.id, props.account.extra?.opencode_go_usage_snapshot, eligible.value], () => {
  generation++
  state.value = null
  error.value = ''
  action.value = ''
  if (eligible.value) void run('load')
}, { immediate: true })
onBeforeUnmount(() => { generation++ })
</script>
