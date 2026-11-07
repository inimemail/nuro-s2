<template>
  <div class="min-w-[9rem] max-w-xs space-y-2" data-testid="ag-quota-windows">
    <p v-if="partial" class="flex items-center gap-1 text-[10px] text-amber-700 dark:text-amber-300" role="status">
      <span class="h-1.5 w-1.5 shrink-0 rounded-full bg-amber-500" />
      {{ t('upgradeQuota.partial') }}
    </p>
    <section
      v-for="(group, index) in groups"
      :key="`${group.key}-${index}`"
      class="rounded-lg border border-gray-200/80 bg-gray-50/60 px-2 py-1.5 dark:border-dark-600 dark:bg-dark-800/50"
      :aria-label="group.name"
    >
      <div class="mb-1 flex items-center justify-between gap-2">
        <span class="truncate text-[10px] font-semibold text-gray-700 dark:text-gray-200" :title="group.description || group.name">{{ group.name || group.key }}</span>
        <span v-if="sources?.summary?.stale" class="shrink-0 text-[9px] text-amber-700 dark:text-amber-300">{{ t('upgradeQuota.stale') }}</span>
      </div>
      <div class="space-y-1">
        <template v-for="(window, windowIndex) in group.windows" :key="`${window.window}-${windowIndex}`">
          <UsageProgressBar
            v-if="window.utilization != null && Number.isFinite(window.utilization)"
            class="quota-window"
            :label="windowLabel(window.window)"
            :utilization="window.utilization"
            :resets-at="window.resets_at"
            color="indigo"
          />
          <div v-else class="flex justify-between gap-3 text-[10px] text-gray-500 dark:text-gray-400">
            <span>{{ windowLabel(window.window) }}</span><span>{{ t('upgradeQuota.unknown') }}</span>
          </div>
        </template>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AntigravityQuotaGroup, AntigravityQuotaSource } from '@/types'
import UsageProgressBar from './UsageProgressBar.vue'

const props = defineProps<{
  groups: AntigravityQuotaGroup[]
  sources?: Record<string, AntigravityQuotaSource> | null
}>()
const { t } = useI18n()
const partial = computed(() => Object.values(props.sources || {}).some(source => source.error || source.stale))
const windowLabel = (window: string) => window === '5h' ? t('upgradeQuota.fiveHour') : window === 'weekly' ? t('upgradeQuota.weekly') : window
</script>

<style scoped>
:deep(.quota-window > .flex > span:first-child) {
  width: 3.25rem;
  white-space: nowrap;
}
</style>
