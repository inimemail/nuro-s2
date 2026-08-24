<template>
  <div v-if="snapshot" class="space-y-1.5" data-testid="monitor-quota-view">
    <div v-if="snapshot.plan_level" class="text-[10px] font-medium text-gray-500 dark:text-gray-400">
      {{ snapshot.plan_level }}
    </div>

    <div v-if="snapshot.success && snapshot.tiers?.length" class="space-y-1">
      <div v-for="(tier, index) in snapshot.tiers" :key="`${tier.window}-${tier.label}-${index}`" class="flex min-w-0 items-center gap-1.5 text-[10px]">
        <span class="w-16 shrink-0 truncate text-gray-500 dark:text-gray-400" :title="tierName(tier)">{{ tierName(tier) }}</span>
        <div class="h-1.5 w-20 shrink-0 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600">
          <div class="h-full rounded-full" :class="barClass(tier.used_percent)" :style="{ width: `${Math.min(100, Math.max(0, tier.used_percent))}%` }" />
        </div>
        <span class="shrink-0 font-semibold tabular-nums" :class="textClass(tier.used_percent)">{{ Math.round(tier.used_percent) }}%</span>
        <span v-if="tier.reset_at" class="truncate text-gray-400" :title="tier.reset_at">{{ formatReset(tier.reset_at) }}</span>
      </div>
    </div>

    <div v-if="snapshot.success && balances.length" class="flex flex-wrap gap-x-2 text-[10px] font-semibold tabular-nums">
      <span v-for="balance in balances" :key="balance.currency" :class="balance.balance <= 0 ? 'text-red-600 dark:text-red-400' : 'text-emerald-700 dark:text-emerald-400'">
        {{ balance.balance.toFixed(2) }} {{ balance.currency }}
      </span>
    </div>

    <p v-if="!snapshot.success" class="max-w-[280px] truncate text-[10px] text-red-600 dark:text-red-400" :title="snapshot.error">
      {{ snapshot.error || t('monitorCommon.quota.unavailable') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { MonitorQuotaSnapshot, MonitorQuotaTier } from '@/api/admin/channelMonitor'

const props = defineProps<{ snapshot?: MonitorQuotaSnapshot | null }>()
const { t } = useI18n()

const balances = computed(() => {
  if (props.snapshot?.balances?.length) return props.snapshot.balances
  if (props.snapshot?.balance != null) return [{ balance: props.snapshot.balance, currency: props.snapshot.currency || '?' }]
  return []
})

function tierName(tier: MonitorQuotaTier) {
  return tier.label ? `${tier.label}/${tier.window}` : tier.window
}

function barClass(percent: number) {
  if (percent >= 90) return 'bg-red-500'
  if (percent >= 75) return 'bg-amber-500'
  return 'bg-emerald-500'
}

function textClass(percent: number) {
  if (percent >= 90) return 'text-red-600 dark:text-red-400'
  if (percent >= 75) return 'text-amber-600 dark:text-amber-400'
  return 'text-emerald-700 dark:text-emerald-400'
}

function formatReset(value: string) {
  const time = new Date(value).getTime()
  if (!Number.isFinite(time)) return value
  const minutes = Math.round((time - Date.now()) / 60_000)
  if (minutes <= 0) return t('monitorCommon.quota.resetSoon')
  if (minutes < 60) return `${minutes}m`
  if (minutes < 2880) return `${Math.round(minutes / 60)}h`
  return new Date(time).toLocaleDateString(undefined, { month: '2-digit', day: '2-digit' })
}
</script>
