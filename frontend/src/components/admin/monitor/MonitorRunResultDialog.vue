<template>
  <BaseDialog
    :show="show"
    :title="t('admin.channelMonitor.runResultTitle')"
    width="normal"
    @close="$emit('close')"
  >
    <div class="space-y-2">
      <div
        v-for="r in results"
        :key="r.model"
        class="flex flex-col items-stretch gap-2 rounded-lg border border-gray-200 px-3 py-2.5 text-sm sm:flex-row sm:items-center sm:justify-between dark:border-dark-600"
      >
        <div class="min-w-0 flex-1">
          <div class="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5">
            <span class="font-medium text-gray-900 dark:text-white">{{ formatMonitorModel(r.model) }}</span>
            <span v-if="r.message" class="min-w-0 break-words text-xs text-gray-500 dark:text-gray-400">{{ r.message }}</span>
          </div>
          <MonitorQuotaView :snapshot="r.quota" class="mt-1.5" />
        </div>
        <div class="flex shrink-0 items-center justify-between gap-2 sm:justify-end">
          <span
            class="inline-flex items-center rounded-full px-2 py-0.5 text-[11px]"
            :class="statusBadgeClass(r.status)"
          >
            {{ statusLabel(r.status) }}
          </span>
          <span class="text-xs text-gray-500 dark:text-gray-400">{{ formatLatency(r.latency_ms) }} ms</span>
        </div>
      </div>
    </div>
    <template #footer>
      <div class="flex justify-end">
        <button @click="$emit('close')" class="btn btn-primary">
          {{ t('common.close') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { CheckResult } from '@/api/admin/channelMonitor'
import BaseDialog from '@/components/common/BaseDialog.vue'
import MonitorQuotaView from '@/components/common/MonitorQuotaView.vue'
import { useChannelMonitorFormat } from '@/composables/useChannelMonitorFormat'

defineProps<{
  show: boolean
  results: CheckResult[]
}>()

defineEmits<{
  (e: 'close'): void
}>()

const { t } = useI18n()
const { statusLabel, statusBadgeClass, formatLatency, formatMonitorModel } = useChannelMonitorFormat()
</script>
