<template>
  <section class="overflow-hidden rounded-lg border border-violet-200 bg-violet-50/70 dark:border-violet-900/70 dark:bg-violet-950/20">
    <div class="flex items-start justify-between gap-4 p-3">
      <div class="flex min-w-0 gap-2.5">
        <span class="mt-0.5 flex h-8 w-8 flex-none items-center justify-center rounded-md bg-white text-violet-600 shadow-sm dark:bg-dark-700 dark:text-violet-300">
          <Icon name="calculator" size="sm" :stroke-width="2" />
        </span>
        <div class="min-w-0">
          <label class="input-label mb-0">{{ t('admin.accounts.downstreamCacheMarkup') }}</label>
          <p class="mt-1 text-xs leading-5 text-violet-700 dark:text-violet-300">
            {{ t('admin.accounts.downstreamCacheMarkupHint') }}
          </p>
        </div>
      </div>
      <button
        type="button"
        data-testid="downstream-cache-markup-toggle"
        :aria-label="t('admin.accounts.downstreamCacheMarkup')"
        :aria-pressed="enabled"
        @click="emit('update:enabled', !enabled)"
        :class="[
          'relative mt-1 inline-flex h-6 w-11 flex-none rounded-full border-2 border-transparent transition-colors focus:outline-none focus:ring-2 focus:ring-violet-500 focus:ring-offset-2',
          enabled ? 'bg-violet-600' : 'bg-gray-200 dark:bg-dark-600'
        ]"
      >
        <span
          :class="[
            'pointer-events-none inline-block h-5 w-5 rounded-full bg-white shadow transition-transform',
            enabled ? 'translate-x-5' : 'translate-x-0'
          ]"
        />
      </button>
    </div>

    <div v-if="enabled" class="border-t border-violet-200/80 p-3 dark:border-violet-900/60">
      <div class="grid gap-3 sm:grid-cols-2">
        <label class="group block min-w-0">
          <span class="mb-1.5 flex items-center justify-between gap-2 text-xs font-semibold text-gray-700 dark:text-gray-200">
            {{ t('admin.accounts.downstreamCacheMarkupThreshold') }}
            <span class="text-[10px] font-medium text-gray-400 dark:text-gray-500">Token</span>
          </span>
          <div class="flex h-10 overflow-hidden rounded-md border border-gray-300 bg-white shadow-sm transition group-focus-within:border-violet-500 group-focus-within:ring-2 group-focus-within:ring-violet-500/15 dark:border-dark-600 dark:bg-dark-800">
            <input
              data-testid="downstream-cache-markup-threshold"
              class="min-w-0 flex-1 border-0 bg-transparent px-3 text-sm font-semibold tabular-nums text-gray-900 outline-none ring-0 placeholder:text-gray-400 focus:border-0 focus:ring-0 dark:text-gray-100"
              type="number"
              min="0"
              step="1"
              inputmode="numeric"
              :aria-label="t('admin.accounts.downstreamCacheMarkupThreshold')"
              :value="thresholdTokens"
              @input="updateThreshold"
            />
            <span class="flex w-16 flex-none items-center justify-center border-l border-gray-200 bg-gray-50 text-[11px] font-medium text-gray-500 dark:border-dark-600 dark:bg-dark-700 dark:text-gray-400">Token</span>
          </div>
        </label>

        <label class="group block min-w-0">
          <span class="mb-1.5 flex items-center justify-between gap-2 text-xs font-semibold text-gray-700 dark:text-gray-200">
            {{ t('admin.accounts.downstreamCacheMarkupPercent') }}
            <span class="text-[10px] font-medium text-gray-400 dark:text-gray-500">%</span>
          </span>
          <div class="flex h-10 overflow-hidden rounded-md border border-gray-300 bg-white shadow-sm transition group-focus-within:border-violet-500 group-focus-within:ring-2 group-focus-within:ring-violet-500/15 dark:border-dark-600 dark:bg-dark-800">
            <input
              data-testid="downstream-cache-markup-percent"
              class="min-w-0 flex-1 border-0 bg-transparent px-3 text-sm font-semibold tabular-nums text-gray-900 outline-none ring-0 placeholder:text-gray-400 focus:border-0 focus:ring-0 dark:text-gray-100"
              type="number"
              min="0"
              step="0.01"
              inputmode="decimal"
              :aria-label="t('admin.accounts.downstreamCacheMarkupPercent')"
              :value="percent"
              @input="updatePercent"
            />
            <span class="flex w-12 flex-none items-center justify-center border-l border-gray-200 bg-gray-50 text-sm font-semibold text-violet-600 dark:border-dark-600 dark:bg-dark-700 dark:text-violet-300">%</span>
          </div>
        </label>
      </div>

      <div class="mt-3 flex flex-wrap items-center gap-2 text-[11px]">
        <span class="font-medium text-gray-500 dark:text-gray-400">{{ t('admin.accounts.downstreamCacheMarkupAllocation') }}</span>
        <span v-for="bucket in buckets" :key="bucket" class="rounded-md bg-white px-2 py-1 text-violet-700 shadow-sm dark:bg-dark-700 dark:text-violet-300">
          {{ t(bucket) }} · 1/3
        </span>
      </div>
      <p v-if="percent <= 0" class="mt-2 text-xs font-medium text-amber-600 dark:text-amber-400">
        {{ t('admin.accounts.downstreamCacheMarkupZeroHint') }}
      </p>
    </div>
  </section>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

defineProps<{
  enabled: boolean
  thresholdTokens: number
  percent: number
}>()

const emit = defineEmits<{
  'update:enabled': [value: boolean]
  'update:thresholdTokens': [value: number]
  'update:percent': [value: number]
}>()

const { t } = useI18n()
const buckets = [
  'admin.accounts.downstreamCacheMarkupInput',
  'admin.accounts.downstreamCacheMarkupRead',
  'admin.accounts.downstreamCacheMarkupOutput'
]

const finiteNonNegative = (value: string): number => {
  const parsed = Number(value)
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : 0
}

const updateThreshold = (event: Event) => {
  emit('update:thresholdTokens', Math.floor(finiteNonNegative((event.target as HTMLInputElement).value)))
}

const updatePercent = (event: Event) => {
  const value = finiteNonNegative((event.target as HTMLInputElement).value)
  emit('update:percent', Math.round(value * 100) / 100)
}
</script>
