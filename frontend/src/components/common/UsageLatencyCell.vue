<template>
  <div class="flex items-stretch gap-2 whitespace-nowrap" data-testid="usage-latency">
    <span
      class="w-1 shrink-0 rounded-full"
      :class="firstToken != null
        ? ['bg-gradient-to-b', LATENCY_BAR_FROM_CLASSES[firstTokenSeverity(firstToken)], LATENCY_BAR_TO_CLASSES[durationSeverity(duration ?? 0)]]
        : duration != null ? LATENCY_BAR_CLASSES[durationSeverity(duration)] : 'bg-gray-300 dark:bg-gray-600'"
      aria-hidden="true"
    ></span>
    <div class="grid grid-cols-[max-content_max-content] items-baseline gap-x-2 gap-y-0.5 text-xs">
      <span class="text-gray-400 dark:text-gray-500">{{ t('usage.latencyFirstToken') }}</span>
      <span class="font-medium tabular-nums" :class="firstToken != null ? LATENCY_TEXT_CLASSES[firstTokenSeverity(firstToken)] : 'text-gray-400 dark:text-gray-500'">{{ formatDuration(firstToken) }}</span>
      <span class="text-gray-400 dark:text-gray-500">{{ t('usage.latencyDuration') }}</span>
      <span class="font-medium tabular-nums" :class="duration != null ? LATENCY_TEXT_CLASSES[durationSeverity(duration)] : 'text-gray-400 dark:text-gray-500'">{{ formatDuration(duration) }}</span>
      <span class="text-gray-400 dark:text-gray-500" :title="t('usage.requestTpsHint')">{{ t('usage.requestTps') }}</span>
      <span class="tabular-nums text-gray-700 dark:text-gray-300" :title="t('usage.requestTpsHint')" data-testid="output-tps">{{ outputTps == null ? '-' : `${outputTps} tok/s` }}</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UsageLog } from '@/types'
import { BILLING_MODE_TOKEN } from '@/utils/billingMode'
import {
  LATENCY_BAR_CLASSES,
  LATENCY_BAR_FROM_CLASSES,
  LATENCY_BAR_TO_CLASSES,
  LATENCY_TEXT_CLASSES,
  durationSeverity,
  firstTokenSeverity,
} from '@/utils/latencyHealth'

type LatencyRow = Partial<Pick<UsageLog,
  'first_token_ms' | 'duration_ms' | 'output_tokens' | 'image_output_tokens' | 'image_count' | 'video_count' | 'billing_mode'
>>

const props = defineProps<{ row: LatencyRow }>()
const { t } = useI18n()

const validDuration = (ms: number | null | undefined): number | null =>
  ms != null && Number.isFinite(ms) && ms >= 0 ? ms : null

const firstToken = computed(() => validDuration(props.row.first_token_ms))
const duration = computed(() => validDuration(props.row.duration_ms))
const outputTps = computed(() => {
  const row = props.row
  if ((row.billing_mode && row.billing_mode !== BILLING_MODE_TOKEN) ||
      (row.image_count ?? 0) > 0 || (row.image_output_tokens ?? 0) > 0 || (row.video_count ?? 0) > 0 ||
      duration.value == null || duration.value <= 0 || row.output_tokens == null ||
      !Number.isFinite(row.output_tokens) || row.output_tokens < 0) return null
  // Request-average output rate includes time spent waiting for the first token.
  const rate = row.output_tokens / (duration.value / 1000)
  return Number.isFinite(rate) ? rate.toFixed(1) : null
})

const formatDuration = (ms: number | null): string => {
  if (ms == null) return '-'
  if (ms < 1000) return `${Math.round(ms)}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(2)}s`
  const totalSec = Math.round(ms / 1000)
  if (totalSec < 3600) return `${Math.floor(totalSec / 60)}m ${totalSec % 60}s`
  return `${Math.floor(totalSec / 3600)}h ${Math.floor((totalSec % 3600) / 60)}m`
}
</script>
