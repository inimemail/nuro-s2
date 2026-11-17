<template>
  <div class="flex items-center gap-2" :title="resetAt">
    <span class="w-6 shrink-0 text-gray-400">{{ label }}</span>
    <div class="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-700"
      role="progressbar" :aria-label="label" :aria-valuenow="percent ?? undefined" :aria-valuetext="percent === null ? t('quotaDisplay.unknown') : undefined" aria-valuemin="0" aria-valuemax="100">
      <div v-if="percent !== null" class="h-full rounded-full" :class="percent >= 90 ? 'bg-red-500' : percent >= 70 ? 'bg-amber-500' : 'bg-emerald-500'" :style="{ width: `${percent}%` }" />
    </div>
    <span class="w-9 shrink-0 text-right tabular-nums text-gray-600 dark:text-gray-300">{{ percent === null ? '—' : `${Math.round(percent)}%` }}</span>
  </div>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { finiteQuotaNumber } from '@/utils/quotaDisplay'
const props = defineProps<{ label: string; value?: unknown; resetAt?: string }>()
const { t } = useI18n()
const percent = computed(() => { const n = finiteQuotaNumber(props.value); return n === null ? null : Math.max(0, Math.min(100, n)) })
</script>
