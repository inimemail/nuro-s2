<template>
  <div class="account-scheduling-field">
    <div class="mb-2.5 flex flex-wrap items-start justify-between gap-2">
      <div class="min-w-0">
        <label class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.groups.form.accountSchedulingStrategy') }}</label>
        <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.form.accountSchedulingStrategyHint') }}</p>
      </div>
      <span class="shrink-0 rounded-md border border-gray-200 bg-gray-50 px-2 py-1 text-[10px] font-semibold text-gray-500 dark:border-dark-600 dark:bg-dark-700 dark:text-gray-400">{{ t('admin.groups.form.strategyScope') }}</span>
    </div>
    <div class="grid overflow-hidden rounded-lg border border-gray-200 bg-gray-50/70 dark:border-dark-600 dark:bg-dark-800/60 md:grid-cols-3" role="group">
      <button
        v-for="option in options"
        :key="option.value"
        type="button"
        class="group relative flex min-h-[70px] items-center gap-2.5 border-b border-gray-200 px-3 py-2.5 text-left transition-colors last:border-b-0 focus-visible:z-10 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500/60 md:min-h-[76px] md:border-b-0 md:border-r md:last:border-r-0"
        :class="modelValue === option.value ? option.activeClass : 'bg-transparent text-gray-600 hover:bg-white dark:border-dark-600 dark:text-gray-300 dark:hover:bg-dark-700/80 dark:hover:text-white'"
        :aria-pressed="modelValue === option.value"
        @click="emit('update:modelValue', option.value)"
      >
        <span class="flex h-7 w-7 flex-none items-center justify-center rounded-md" :class="modelValue === option.value ? option.iconClass : 'bg-white text-gray-400 dark:bg-dark-700 dark:text-gray-400'">
          <Icon :name="option.icon" size="sm" :stroke-width="2" />
        </span>
        <span class="min-w-0 flex-1">
          <span class="block truncate text-[13px] font-semibold leading-5">{{ t(option.label) }}</span>
          <span class="mt-0.5 block truncate text-[11px] leading-4 text-gray-500 dark:text-gray-400">{{ t(option.shortHint) }}</span>
        </span>
        <span v-if="modelValue === option.value" class="absolute right-2 top-2 flex h-4 w-4 items-center justify-center rounded-full bg-emerald-500 text-white dark:bg-emerald-400 dark:text-emerald-950">
          <Icon name="check" size="xs" :stroke-width="2.5" />
        </span>
      </button>
    </div>
    <div class="mt-2 flex items-start gap-2 rounded-md border border-primary-100 bg-primary-50/55 px-2.5 py-2 text-[11px] leading-5 text-primary-800 dark:border-primary-900/50 dark:bg-primary-900/20 dark:text-primary-200">
      <Icon name="infoCircle" size="sm" class="mt-px flex-none" :stroke-width="2" />
      <p class="min-w-0">{{ t(selectedOption.hint) }}</p>
    </div>
    <div v-if="modelValue !== 'strict_priority'" class="mt-2 flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-gray-100 pt-2.5 dark:border-dark-700">
      <div class="flex min-w-0 flex-1 items-center gap-2.5">
        <button
          type="button"
          data-testid="adaptive-ttft-switch"
          class="relative inline-flex h-5 w-9 flex-none rounded-full transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500/60"
          :class="ttftSwitchEnabled ? 'bg-emerald-500' : 'bg-gray-300 dark:bg-dark-600'"
          role="switch"
          :aria-checked="ttftSwitchEnabled"
          :aria-label="t('admin.groups.form.adaptiveTTFTSwitch')"
          @click="emit('update:ttftSwitchEnabled', !ttftSwitchEnabled)"
        >
          <span
            class="mt-0.5 h-4 w-4 rounded-full bg-white shadow-sm transition-transform"
            :class="ttftSwitchEnabled ? 'translate-x-[18px]' : 'translate-x-0.5'"
          />
        </button>
        <div class="min-w-0">
          <p class="text-xs font-medium text-gray-700 dark:text-gray-200">{{ t('admin.groups.form.adaptiveTTFTSwitch') }}</p>
          <p class="text-[11px] leading-4 text-gray-500 dark:text-gray-400">{{ t(ttftSwitchHint) }}</p>
        </div>
      </div>
      <div class="flex flex-none flex-wrap items-center gap-x-3 gap-y-2">
        <label class="flex items-center gap-1.5 text-xs text-gray-600 dark:text-gray-300">
          <span>{{ t('admin.groups.form.adaptiveTTFTThreshold') }}</span>
          <input
            data-testid="adaptive-ttft-threshold"
            type="number"
            inputmode="numeric"
            min="1"
            max="3600"
            step="1"
            class="h-8 w-20 rounded-md border border-gray-300 bg-white px-2 text-right text-sm text-gray-900 outline-none focus:border-primary-500 focus:ring-1 focus:ring-primary-500 disabled:cursor-not-allowed disabled:bg-gray-100 disabled:text-gray-400 dark:border-dark-600 dark:bg-dark-800 dark:text-white dark:disabled:bg-dark-700"
            :disabled="!ttftSwitchEnabled"
            :value="ttftSwitchThresholdSeconds"
            @input="updateThreshold"
          />
          <span>{{ t('admin.groups.form.seconds') }}</span>
        </label>
        <label class="flex items-center gap-1.5 text-xs text-gray-600 dark:text-gray-300">
          <span>{{ t('admin.groups.form.adaptiveHealthFreshness') }}</span>
          <input
            data-testid="adaptive-health-freshness"
            type="number"
            inputmode="numeric"
            min="1"
            max="120"
            step="1"
            class="h-8 w-16 rounded-md border border-gray-300 bg-white px-2 text-right text-sm text-gray-900 outline-none focus:border-primary-500 focus:ring-1 focus:ring-primary-500 dark:border-dark-600 dark:bg-dark-800 dark:text-white"
            :value="healthFreshnessMinutes"
            @input="updateFreshness"
          />
          <span>{{ t('admin.groups.form.minutes') }}</span>
        </label>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

type Strategy = 'strict_priority' | 'health_first' | 'health_cost_balanced'
const props = withDefaults(defineProps<{
  modelValue?: Strategy
  ttftSwitchEnabled?: boolean
  ttftSwitchThresholdSeconds?: number
  healthFreshnessMinutes?: number
}>(), {
  modelValue: 'strict_priority',
  ttftSwitchEnabled: true,
  ttftSwitchThresholdSeconds: 60,
  healthFreshnessMinutes: 15,
})
const emit = defineEmits<{
  (event: 'update:modelValue', value: Strategy): void
  (event: 'update:ttftSwitchEnabled', value: boolean): void
  (event: 'update:ttftSwitchThresholdSeconds', value: number): void
  (event: 'update:healthFreshnessMinutes', value: number): void
}>()
const { t } = useI18n()
const options = [
  { value: 'strict_priority' as const, icon: 'shield' as const, label: 'admin.groups.form.strictPriority', shortHint: 'admin.groups.form.strictPriorityShortHint', hint: 'admin.groups.form.strictPriorityHint', activeClass: 'border-emerald-300 bg-emerald-50/75 text-emerald-900 dark:border-emerald-700 dark:bg-emerald-900/25 dark:text-emerald-100', iconClass: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/60 dark:text-emerald-200' },
  { value: 'health_first' as const, icon: 'bolt' as const, label: 'admin.groups.form.healthLeading', shortHint: 'admin.groups.form.healthLeadingShortHint', hint: 'admin.groups.form.healthLeadingHint', activeClass: 'border-emerald-300 bg-emerald-50/75 text-emerald-900 dark:border-emerald-700 dark:bg-emerald-900/25 dark:text-emerald-100', iconClass: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/60 dark:text-emerald-200' },
  { value: 'health_cost_balanced' as const, icon: 'chartBar' as const, label: 'admin.groups.form.healthCostBalanced', shortHint: 'admin.groups.form.healthCostBalancedShortHint', hint: 'admin.groups.form.healthCostBalancedHint', activeClass: 'border-emerald-300 bg-emerald-50/75 text-emerald-900 dark:border-emerald-700 dark:bg-emerald-900/25 dark:text-emerald-100', iconClass: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/60 dark:text-emerald-200' },
]
const selectedOption = computed(() => options.find(option => option.value === props.modelValue) ?? options[0])
const ttftSwitchHint = computed(() => props.modelValue === 'health_cost_balanced'
  ? 'admin.groups.form.adaptiveTTFTCostBalancedHint'
  : 'admin.groups.form.adaptiveTTFTHealthFirstHint')

const updateThreshold = (event: Event) => {
  const value = Number((event.target as HTMLInputElement).value)
  if (Number.isInteger(value) && value >= 1 && value <= 3600) {
    emit('update:ttftSwitchThresholdSeconds', value)
  }
}

const updateFreshness = (event: Event) => {
  const value = Number((event.target as HTMLInputElement).value)
  if (Number.isInteger(value) && value >= 1 && value <= 120) {
    emit('update:healthFreshnessMinutes', value)
  }
}
</script>
