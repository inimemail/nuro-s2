<template>
  <div v-if="eligible" class="mt-1 w-40 space-y-1 text-[10px]">
    <div class="flex items-center justify-between gap-2 text-gray-500 dark:text-gray-400">
      <span class="flex items-center gap-1 font-medium"><Icon name="chart" size="xs" />{{ account.platform }}</span>
      <button type="button" class="rounded p-0.5 text-primary-600 transition hover:bg-primary-50 disabled:opacity-50 dark:text-primary-400 dark:hover:bg-dark-700" :disabled="loading" :title="t('admin.accounts.cnProviders.refresh', '刷新')" @click="refresh(true)">
        <Icon name="refresh" size="xs" :class="{ 'animate-spin': loading }" />
      </button>
    </div>
    <template v-if="isCodingPlan">
      <QuotaBar v-for="tier in quotaTiers" :key="tier.window" :label="tier.window === '5h' ? '5h' : '7d'" :value="tier.used_percent" />
    </template>
    <template v-else-if="balance">
      <div class="flex items-center justify-between gap-2 rounded bg-gray-50 px-1.5 py-1 dark:bg-dark-800">
        <span class="text-gray-400">{{ balance.currency || 'USD' }}</span>
        <span class="font-medium tabular-nums" :class="balance.available === false ? 'text-red-500' : 'text-gray-600 dark:text-gray-300'">{{ balance.balance.toFixed(2) }}</span>
      </div>
    </template>
    <span v-if="probeError" class="block truncate text-amber-600 dark:text-amber-400" :title="probeError">{{ t('admin.accounts.cnProviders.probeFailed', '探测失败，显示上次快照') }}</span>
  </div>
</template>

<script setup lang="ts">
import { computed, defineComponent, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { adminAPI } from '@/api/admin'
import type { Account } from '@/types'
import type { CNProviderBalanceResult, CNProviderQuotaTier } from '@/api/admin/accounts'

type CNProbeResult = CNProviderBalanceResult | { provider: string; success: boolean; tiers?: CNProviderQuotaTier[]; error?: string }
const CN_PROBE_CACHE_TTL = 5 * 60 * 1000
const cnProbeCache = new Map<string, { result: CNProbeResult; ts: number }>()
const cnProbeInFlight = new Map<string, Promise<CNProbeResult>>()

const props = defineProps<{ account: Account }>()
const { t } = useI18n()
const quotaTiers = ref<CNProviderQuotaTier[]>([])
const balance = ref<CNProviderBalanceResult | null>(null)
const loading = ref(false)
const probeError = ref('')
const eligible = computed(() => ['kimi', 'zhipu', 'deepseek'].includes(props.account.platform) && props.account.type === 'apikey')
const isCodingPlan = computed(() => {
  if (!['kimi', 'zhipu'].includes(props.account.platform)) return false
  const extraMode = props.account.extra?.cn_billing_mode
  const legacyMode = props.account.credentials?.account_mode
  return extraMode === 'coding_plan' || legacyMode === 'coding' || legacyMode === 'coding_plan'
})
const snapshot = computed(() => props.account.extra || {})
const QuotaBar = defineComponent({ props: { label: { type: String, required: true }, value: { type: Number, default: 0 } }, setup(p) { return () => h('div', { class: 'flex items-center gap-1.5' }, [h('span', { class: 'w-5 shrink-0 text-gray-400' }, p.label), h('div', { class: 'h-1 flex-1 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-700' }, [h('div', { class: p.value >= 90 ? 'h-full bg-red-500' : p.value >= 70 ? 'h-full bg-amber-500' : 'h-full bg-emerald-500', style: { width: `${Math.max(0, Math.min(100, p.value))}%` } })]), h('span', { class: 'w-7 text-right tabular-nums text-gray-500 dark:text-gray-400' }, `${Math.round(p.value)}%`)]) } })
function loadSnapshot() {
  const provider = props.account.platform
  if (isCodingPlan.value) {
    const tiers: CNProviderQuotaTier[] = []
    const five = Number(snapshot.value[`${provider}_5h_used_percent`])
    const weekly = Number(snapshot.value[`${provider}_weekly_used_percent`])
    if (Number.isFinite(five)) tiers.push({ window: '5h', used_percent: five })
    if (Number.isFinite(weekly)) tiers.push({ window: 'weekly', used_percent: weekly })
    quotaTiers.value = tiers
  } else {
    const value = Number(snapshot.value[`${provider}_balance`])
    if (Number.isFinite(value)) balance.value = { provider, success: true, balance: value, currency: String(snapshot.value[`${provider}_balance_currency`] || 'USD'), available: snapshot.value[`${provider}_balance_available`] !== false }
  }
}

function applyProbeResult(result: CNProbeResult, mode: 'quota' | 'balance') {
  if (mode === 'quota') {
    const quota = result as { tiers?: CNProviderQuotaTier[] }
    if (result.success && quota.tiers) quotaTiers.value = quota.tiers
  } else if (result.success) {
    balance.value = result as CNProviderBalanceResult
  }
}

async function refresh(force = false) {
  if (!eligible.value) return
  loading.value = true; probeError.value = ''
  const mode: 'quota' | 'balance' = isCodingPlan.value ? 'quota' : 'balance'
  const cacheKey = `${props.account.id}:${mode}`
  try {
    const cached = cnProbeCache.get(cacheKey)
    if (!force && cached && Date.now() - cached.ts < CN_PROBE_CACHE_TTL) {
      applyProbeResult(cached.result, mode)
      if (!cached.result.success) probeError.value = cached.result.error || 'probe failed'
      return
    }
    let pending = cnProbeInFlight.get(cacheKey)
    if (!pending) {
      pending = mode === 'quota'
        ? adminAPI.accounts.getCNProviderQuota(props.account.id)
        : adminAPI.accounts.getCNProviderBalance(props.account.id)
      cnProbeInFlight.set(cacheKey, pending)
    }
    let result: CNProbeResult
    try {
      result = await pending
    } finally {
      if (cnProbeInFlight.get(cacheKey) === pending) cnProbeInFlight.delete(cacheKey)
    }
    cnProbeCache.set(cacheKey, { result, ts: Date.now() })
    applyProbeResult(result, mode)
    if (!result.success) probeError.value = result.error || 'probe failed'
  } catch (error) { probeError.value = error instanceof Error ? error.message : 'probe failed' } finally { loading.value = false }
}
onMounted(() => { loadSnapshot(); void refresh() })
</script>
