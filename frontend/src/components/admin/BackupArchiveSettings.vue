<template>
  <section class="mt-5 border-t border-gray-200 pt-4 dark:border-dark-600">
    <label class="flex cursor-pointer items-start gap-3">
      <input type="checkbox" class="mt-1 rounded border-gray-300 text-primary-600 focus:ring-primary-500" :checked="value.enabled" @change="update({ enabled: ($event.target as HTMLInputElement).checked })" />
      <span class="min-w-0">
        <span class="block text-sm font-semibold text-gray-900 dark:text-white">{{ t('backupArchive.title') }}</span>
        <span class="mt-1 block text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('backupArchive.description') }}</span>
      </span>
    </label>
    <div v-if="value.enabled" class="mt-4 space-y-5">
      <div>
        <p class="mb-2 text-xs font-medium text-gray-700 dark:text-gray-300">{{ t('backupArchive.days') }}</p>
        <div class="grid max-w-md grid-cols-7 gap-1.5" role="group" :aria-label="t('backupArchive.days')">
          <button v-for="day in 31" :key="day" type="button" :aria-pressed="value.days.includes(day)" class="rounded-lg border py-2 text-xs font-medium transition-colors focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-1" :class="value.days.includes(day) ? 'border-primary-500 bg-primary-50 text-primary-700 dark:bg-primary-500/15 dark:text-primary-300' : 'border-gray-200 bg-white text-gray-600 hover:border-primary-300 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-300'" @click="toggleDay(day)">{{ day }}</button>
        </div>
        <label class="mt-3 inline-flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
          <input type="checkbox" :checked="value.include_month_end" @change="update({ include_month_end: ($event.target as HTMLInputElement).checked })" />{{ t('backupArchive.monthEnd') }}
        </label>
        <p class="mt-2 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('backupArchive.dateHint') }}</p>
      </div>
      <div class="max-w-xs">
        <label class="mb-1 block text-xs font-medium text-gray-700 dark:text-gray-300">{{ t('backupArchive.retainCount') }}</label>
        <input type="number" min="0" max="10000" step="1" class="input w-full" :value="value.retain_count" @change="updateRetainCount" />
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('backupArchive.retentionHint') }}</p>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { BackupMonthlyArchiveConfig } from '@/api/admin/backup'
const props = defineProps<{ modelValue?: BackupMonthlyArchiveConfig }>()
const emit = defineEmits<{ 'update:modelValue': [value: BackupMonthlyArchiveConfig] }>()
const { t } = useI18n()
const value = computed(() => props.modelValue ?? { enabled: false, days: [1], include_month_end: false, retain_count: 0 })
function update(patch: Partial<BackupMonthlyArchiveConfig>) { emit('update:modelValue', { ...value.value, ...patch }) }
function updateRetainCount(event: Event) {
  const input = event.target as HTMLInputElement
  const raw = input.value.trim()
  const count = Number(raw)
  if (raw === '' || !Number.isInteger(count) || count < 0 || count > 10000) {
    input.value = String(value.value.retain_count)
    return
  }
  update({ retain_count: count })
}
function toggleDay(day: number) {
  update({ days: value.value.days.includes(day) ? value.value.days.filter(d => d !== day) : [...value.value.days, day].sort((a, b) => a - b) })
}
</script>
