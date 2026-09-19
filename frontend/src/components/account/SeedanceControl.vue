<template>
  <div class="rounded-xl border p-4 transition-colors" :class="modelValue ? 'border-primary-200 bg-primary-50/60 dark:border-primary-800 dark:bg-primary-950/20' : 'border-gray-200 bg-gray-50/70 dark:border-dark-600 dark:bg-dark-800/40'">
    <label class="flex cursor-pointer items-center justify-between gap-4">
      <span class="min-w-0">
        <span class="block text-sm font-semibold text-gray-900 dark:text-gray-100">Seedance</span>
        <span class="mt-1 block text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.seedanceDescription') }}</span>
      </span>
      <input type="checkbox" class="h-5 w-5 shrink-0 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-500"
        data-testid="seedance-enabled" :checked="modelValue" @change="$emit('update:modelValue', ($event.target as HTMLInputElement).checked)" />
    </label>
    <div v-if="modelValue" class="mt-3 border-t border-primary-200/70 pt-3 text-xs leading-5 text-primary-700 dark:border-primary-800 dark:text-primary-300">
      {{ t('admin.accounts.openai.seedanceRequirements') }}
      <code class="mt-2 block break-all rounded-lg bg-white/70 px-3 py-2 text-[11px] dark:bg-dark-900/60">POST /api/v3/contents/generations/tasks</code>
    </div>
  </div>
</template>
<script setup lang="ts">
import { useI18n } from 'vue-i18n'
defineProps<{ modelValue: boolean }>()
defineEmits<{ 'update:modelValue': [value: boolean] }>()
const { t } = useI18n()
</script>
