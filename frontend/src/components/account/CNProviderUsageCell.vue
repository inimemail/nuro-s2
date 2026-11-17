<template>
  <div v-if="eligible" class="mt-1 w-40 max-w-full space-y-1.5 rounded-lg border border-gray-100 bg-gray-50/60 p-2 text-[10px] dark:border-dark-700 dark:bg-dark-800/60" :aria-busy="loading">
    <div class="flex items-center justify-between gap-2 text-gray-500 dark:text-gray-400">
      <span class="flex min-w-0 items-center gap-1 font-medium"><Icon name="chart" size="xs" /><span class="truncate">{{ account.platform }}</span></span>
      <button type="button" class="rounded p-0.5 text-primary-600 transition hover:bg-primary-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:opacity-50 dark:text-primary-400 dark:hover:bg-dark-700" :disabled="loading" :aria-label="t('quotaDisplay.refresh')" :title="t('quotaDisplay.refresh')" @click="refresh(true)">
        <Icon name="refresh" size="xs" :class="{ 'animate-spin motion-reduce:animate-none': loading }" />
      </button>
    </div>
    <template v-if="isCodingPlan">
      <QuotaUsageBar v-for="tier in quotaTiers" :key="tier.window" :label="tier.window === '5h' ? '5h' : tier.window === 'weekly' ? '7d' : '1mo'" :value="tier.used_percent" :reset-at="tier.reset_at" />
      <span v-if="!quotaTiers.length" class="text-gray-400">{{ t('quotaDisplay.unknown') }} · —</span>
    </template>
    <div v-else class="flex items-center justify-between gap-2">
      <span class="text-gray-400">{{ balance?.currency || 'USD' }}</span>
      <span class="font-medium tabular-nums" :class="balance?.available === false ? 'text-red-500' : 'text-gray-600 dark:text-gray-300'">{{ balance ? balance.balance.toFixed(2) : '—' }}</span>
    </div>
    <span v-if="probeError" class="block truncate text-amber-600 dark:text-amber-400" role="status" :title="probeError">{{ t('quotaDisplay.failed') }}</span>
    <span v-else-if="fetchedAt" class="block truncate text-gray-400" :title="fetchedAt">{{ t('quotaDisplay.updated') }} · {{ fetchedAt }}</span>
  </div>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import QuotaUsageBar from './QuotaUsageBar.vue'
import { adminAPI } from '@/api/admin'
import { extractSafeApiErrorMessage, isApiRequestCancelled } from '@/utils/apiError'
import { finiteQuotaNumber } from '@/utils/quotaDisplay'
import type { Account } from '@/types'
import type { CNProviderBalanceResult, CNProviderQuotaTier } from '@/api/admin/accounts'
const props = defineProps<{ account: Account }>()
const { t } = useI18n()
const quotaTiers = ref<CNProviderQuotaTier[]>([])
const balance = ref<CNProviderBalanceResult | null>(null)
const loading = ref(false)
const probeError = ref('')
const fetchedAt = ref('')
let generation = 0
let controller: AbortController | null = null
let successAt = 0
let failedAt = 0
let loadedSnapshot = ''
const baseURL = computed(() => String(props.account.credentials?.base_url || '').trim().replace(/\/$/, ''))
const isCodingPlan = computed(() => {
  if (props.account.platform === 'opencode_go') return (props.account.credentials?.account_mode ?? props.account.extra?.account_mode) !== 'zen'
  if (!['kimi', 'zhipu', 'minimax'].includes(props.account.platform)) return false
  // Match GetCNBillingMode: an explicit local mode overrides legacy credentials.
  const mode = String(props.account.extra?.cn_billing_mode ?? '').trim()
  if (mode === 'payg' || mode === 'coding_plan') return mode === 'coding_plan'
  return ['coding', 'coding_plan'].includes(String(props.account.credentials?.account_mode ?? '').trim())
})
const eligible = computed(() => ['kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'].includes(props.account.platform) && props.account.type === 'apikey' && !['https://ollama.com', 'https://ollama.com/v1'].includes(baseURL.value.toLowerCase()) && (!['minimax', 'opencode_go'].includes(props.account.platform) || isCodingPlan.value))
const identity = computed(() => {
  const a = props.account, c = a.credentials || {}, e = a.extra || {}
  return JSON.stringify([a.id, a.platform, a.type, c.api_key, baseURL.value, c.account_mode, e.account_mode,
    e.cn_api_mode, e.cn_billing_mode, c.api_protocol, e.cn_api_base_urls, c.api_base_urls,
    c.zhipu_organization, c.zhipu_project, c.header_override_enabled, c.header_overrides, a.proxy_id, eligible.value])
})
const snapshot = computed(() => {
  const p = props.account.platform, e = props.account.extra || {}
  return JSON.stringify(['5h', 'weekly', 'monthly'].flatMap(w => [e[`${p}_${w}_used_percent`], e[`${p}_${w}_reset_at`]]).concat([e[`${p}_balance`], e[`${p}_balance_currency`], e[`${p}_balance_available`]]))
})
function loadSnapshot() {
  loadedSnapshot = snapshot.value
  if (!eligible.value) return
  const provider = props.account.platform, extra = props.account.extra || {}
  if (isCodingPlan.value) {
    quotaTiers.value = (['5h', 'weekly', 'monthly'] as const).flatMap(window => {
      const n = finiteQuotaNumber(extra[`${provider}_${window}_used_percent`])
      return n === null ? [] : [{ window, used_percent: n, reset_at: String(extra[`${provider}_${window}_reset_at`] || '') }]
    })
  } else {
    const n = finiteQuotaNumber(extra[`${provider}_balance`])
    balance.value = n === null ? null : { provider, success: true, balance: n, currency: String(extra[`${provider}_balance_currency`] || 'USD'), available: extra[`${provider}_balance_available`] !== false }
  }
}
async function refresh(force = false) {
  if (!eligible.value || loading.value) return
  const now = Date.now()
  if (!force && ((successAt > 0 && now - successAt < 300000) || (failedAt > 0 && now - failedAt < 5000))) return
  const current = generation, id = props.account.id, quota = isCodingPlan.value
  const request = new AbortController()
  controller = request
  loading.value = true
  probeError.value = ''
  try {
    const result = quota ? await adminAPI.accounts.getCNProviderQuota(id, { signal: request.signal }) : await adminAPI.accounts.getCNProviderBalance(id, { signal: request.signal })
    if (current !== generation || request.signal.aborted) return
    if (!result.success) throw new Error(result.error || t('quotaDisplay.failed'))
    if ('tiers' in result && quota) {
      quotaTiers.value = (result.tiers || []).filter(tier => ['5h', 'weekly', 'monthly'].includes(tier.window) && finiteQuotaNumber(tier.used_percent) !== null)
    } else if (!quota && 'balance' in result && finiteQuotaNumber(result.balance) !== null) {
      balance.value = { ...result, balance: finiteQuotaNumber(result.balance)! }
    } else throw new Error(t('quotaDisplay.unknown'))
    successAt = Date.now()
    failedAt = 0
    const date = result.fetched_at ? new Date(result.fetched_at * 1000) : null
    fetchedAt.value = date && Number.isFinite(date.getTime()) ? date.toLocaleString() : ''
  } catch (error) {
    if (current !== generation || request.signal.aborted || isApiRequestCancelled(error)) return
    failedAt = Date.now()
    probeError.value = extractSafeApiErrorMessage(error, t('quotaDisplay.failed'))
  } finally {
    if (current === generation) { loading.value = false; controller = null }
  }
}
watch(identity, () => {
  generation++
  controller?.abort()
  controller = null
  loading.value = false
  quotaTiers.value = []
  balance.value = null
  fetchedAt.value = ''
  probeError.value = ''
  successAt = failedAt = 0
  loadSnapshot()
  void refresh()
}, { immediate: true, flush: 'sync' })
watch(snapshot, () => {
  if (snapshot.value === loadedSnapshot) return
  // A new saved snapshot supersedes an in-flight result, without starting another probe.
  generation++
  controller?.abort()
  controller = null
  loading.value = false
  loadSnapshot()
  probeError.value = ''
  fetchedAt.value = ''
})
onBeforeUnmount(() => { generation++; controller?.abort() })
</script>
