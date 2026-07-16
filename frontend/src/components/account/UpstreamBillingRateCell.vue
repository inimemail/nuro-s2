<template>
  <div v-if="eligible" class="flex min-w-[6.5rem] items-center gap-1.5">
    <div class="min-w-0">
      <div class="font-mono text-sm font-medium text-gray-800 dark:text-gray-200">
        {{ displayedRate }}
      </div>
      <div :class="statusClass" class="truncate text-[10px]" :title="statusTitle">
        {{ statusLabel }}
      </div>
    </div>
    <button
      type="button"
      class="inline-flex h-7 w-7 flex-shrink-0 items-center justify-center rounded text-primary-600 transition-colors hover:bg-primary-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-primary-400 dark:hover:bg-primary-900/30"
      :disabled="probing"
      :aria-label="t('admin.accounts.upstreamBilling.manualProbe')"
      :title="t('admin.accounts.upstreamBilling.manualProbe')"
      @click="$emit('probe')"
    >
      <Icon name="refresh" size="xs" :class="{ 'animate-spin': probing }" />
    </button>
  </div>
  <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import type { Account, UpstreamBillingProbeSnapshot } from '@/types'

const props = defineProps<{
  account: Account
  now: number
  probing?: boolean
}>()

defineEmits<{
  (event: 'probe'): void
}>()

const { t } = useI18n()
const eligible = computed(() => props.account.platform === 'openai' && props.account.type === 'apikey')
const snapshot = computed<UpstreamBillingProbeSnapshot | undefined>(
  () => props.account.extra?.upstream_billing_probe
)
const freshUntil = computed(() => {
  const value = snapshot.value?.fresh_until
  return typeof value === 'string' ? Date.parse(value) : Number.NaN
})
const stale = computed(() => {
  if (!snapshot.value) return false
  return !Number.isFinite(freshUntil.value) || props.now >= freshUntil.value
})
const displayedRate = computed(() => {
  if (snapshot.value?.status !== 'ok' || stale.value) return '-'
  const value = snapshot.value.data?.effective_rate_multiplier
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
    ? `${Number(value.toPrecision(12))}x`
    : '-'
})
const statusLabel = computed(() => {
  if (!snapshot.value) return t('admin.accounts.upstreamBilling.notProbed')
  if (snapshot.value.status === 'unsupported') return t('admin.accounts.upstreamBilling.unsupported')
  if (snapshot.value.status === 'failed') return t('admin.accounts.upstreamBilling.failed')
  if (stale.value) return t('admin.accounts.upstreamBilling.stale')
  return t('admin.accounts.upstreamBilling.observed')
})
const statusClass = computed(() => {
  if (snapshot.value?.status === 'failed') return 'text-red-600 dark:text-red-400'
  if (snapshot.value?.status === 'unsupported') return 'text-gray-500 dark:text-gray-400'
  if (stale.value) return 'text-amber-600 dark:text-amber-400'
  if (snapshot.value?.status === 'ok') return 'text-emerald-600 dark:text-emerald-400'
  return 'text-gray-400 dark:text-gray-500'
})
const statusTitle = computed(() => {
  const value = snapshot.value?.received_at || snapshot.value?.last_attempt_at
  if (!value) return statusLabel.value
  const timestamp = Date.parse(value)
  if (!Number.isFinite(timestamp)) return statusLabel.value
  return `${statusLabel.value} · ${new Date(timestamp).toLocaleString()}`
})
</script>
