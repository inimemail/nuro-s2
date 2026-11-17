<template>
  <div v-if="eligible" class="mt-1 w-40 max-w-full space-y-1.5 rounded-lg border border-gray-100 bg-gray-50/60 p-2 text-[10px] dark:border-dark-700 dark:bg-dark-800/60" :aria-busy="loading">
    <div class="flex items-center justify-between gap-2 text-gray-500 dark:text-gray-400">
      <span class="flex items-center gap-1 font-medium"><Icon name="cloud" size="xs" /> Ollama</span>
      <button v-if="state?.configured" type="button" class="rounded p-0.5 text-primary-600 hover:bg-primary-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:opacity-50 dark:text-primary-400 dark:hover:bg-dark-700" :disabled="loading" :aria-label="t('admin.accounts.ollamaRefreshNow')" :title="t('admin.accounts.ollamaRefreshNow')" @click="load(true)">
        <Icon name="refresh" size="xs" :class="{ 'animate-spin motion-reduce:animate-none': loading }" />
      </button>
    </div>
    <QuotaUsageBar :label="t('admin.accounts.ollamaFiveHourShort')" :value="state?.snapshot?.data?.five_hour?.used_percent" />
    <QuotaUsageBar :label="t('admin.accounts.ollamaSevenDayShort')" :value="state?.snapshot?.data?.seven_day?.used_percent" />
    <span v-if="error || ['error', 'failed'].includes(state?.snapshot?.status || '')" class="block truncate text-amber-600 dark:text-amber-400" role="status" :title="error">{{ t('quotaDisplay.failed') }}</span>
    <span v-else-if="state?.snapshot?.fetched_at" class="block truncate text-gray-400" :title="state.snapshot.fetched_at">{{ t('quotaDisplay.updated') }} · {{ state.snapshot.fetched_at }}</span>
  </div>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { extractSafeApiErrorMessage, isApiRequestCancelled } from '@/utils/apiError'
import Icon from '@/components/icons/Icon.vue'
import QuotaUsageBar from './QuotaUsageBar.vue'
import type { Account, OllamaCloudUsageState } from '@/types'
const props = defineProps<{ account: Account }>()
const { t } = useI18n()
const state = ref<OllamaCloudUsageState | null>(null)
const loading = ref(false)
const error = ref('')
let generation = 0
let snapshotRevision = 0
let controller: AbortController | null = null
const baseURL = computed(() => String(props.account.credentials?.base_url || '').replace(/\/$/, '').toLowerCase())
const eligible = computed(() => props.account.type === 'apikey' && ['openai', 'anthropic', 'kimi', 'zhipu', 'deepseek', 'minimax'].includes(props.account.platform) && ['https://ollama.com', 'https://ollama.com/v1'].includes(baseURL.value))
const identity = computed(() => JSON.stringify([props.account.id, props.account.platform, props.account.type, props.account.credentials?.api_key, props.account.extra?.ollama_cloud_usage_session, baseURL.value, eligible.value]))
async function load(refresh = false) {
  if (!eligible.value || loading.value || (refresh && !state.value?.configured)) return
  const current = generation, revision = snapshotRevision, id = props.account.id, request = new AbortController()
  controller = request
  loading.value = true
  error.value = ''
  try {
    const result = refresh ? await adminAPI.accounts.refreshOllamaCloudUsage(id, { signal: request.signal }) : await adminAPI.accounts.getOllamaCloudUsage(id, { signal: request.signal })
    if (current === generation && !request.signal.aborted && result.account_id === id) {
      // A failed provider attempt may omit data. Preserve only this account's
      // last successful values, while displaying the failure status.
      applyState(revision !== snapshotRevision && props.account.extra?.ollama_cloud_usage_snapshot
        ? { ...result, snapshot: props.account.extra.ollama_cloud_usage_snapshot as OllamaCloudUsageState['snapshot'] }
        : result)
    }
  } catch (err) {
    if (current === generation && !request.signal.aborted && !isApiRequestCancelled(err)) error.value = extractSafeApiErrorMessage(err, t('quotaDisplay.failed'))
  } finally {
    if (current === generation) { loading.value = false; controller = null }
  }
}
function applyState(result: OllamaCloudUsageState) {
  const previousData = state.value?.snapshot?.data
  state.value = ['failed', 'error'].includes(result.snapshot?.status || '') && !result.snapshot?.data && previousData
    ? { ...result, snapshot: { ...result.snapshot!, data: previousData } }
    : result
}
watch(identity, () => {
  generation++
  controller?.abort()
  controller = null
  loading.value = false
  state.value = null
  error.value = ''
  void load()
}, { immediate: true, flush: 'sync' })
watch(() => props.account.extra?.ollama_cloud_usage_snapshot, snapshot => {
  if (!eligible.value || !snapshot || typeof snapshot !== 'object') return
  snapshotRevision++
  // Keep the initial GET to learn configuration/eligibility, then use the newer snapshot.
  if (!state.value) return
  generation++
  controller?.abort()
  controller = null
  loading.value = false
  error.value = ''
  applyState({ ...state.value, snapshot: snapshot as OllamaCloudUsageState['snapshot'] })
}, { deep: true })
onBeforeUnmount(() => { generation++; controller?.abort() })
</script>
