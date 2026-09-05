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
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

type Strategy = 'strict_priority' | 'health_first' | 'health_cost_balanced'
const props = withDefaults(defineProps<{ modelValue?: Strategy }>(), { modelValue: 'strict_priority' })
const emit = defineEmits<{ (event: 'update:modelValue', value: Strategy): void }>()
const { t } = useI18n()
const options = [
  { value: 'strict_priority' as const, icon: 'shield' as const, label: 'admin.groups.form.strictPriority', shortHint: 'admin.groups.form.strictPriorityShortHint', hint: 'admin.groups.form.strictPriorityHint', activeClass: 'border-emerald-300 bg-emerald-50/75 text-emerald-900 dark:border-emerald-700 dark:bg-emerald-900/25 dark:text-emerald-100', iconClass: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/60 dark:text-emerald-200' },
  { value: 'health_first' as const, icon: 'bolt' as const, label: 'admin.groups.form.healthLeading', shortHint: 'admin.groups.form.healthLeadingShortHint', hint: 'admin.groups.form.healthLeadingHint', activeClass: 'border-emerald-300 bg-emerald-50/75 text-emerald-900 dark:border-emerald-700 dark:bg-emerald-900/25 dark:text-emerald-100', iconClass: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/60 dark:text-emerald-200' },
  { value: 'health_cost_balanced' as const, icon: 'chartBar' as const, label: 'admin.groups.form.healthCostBalanced', shortHint: 'admin.groups.form.healthCostBalancedShortHint', hint: 'admin.groups.form.healthCostBalancedHint', activeClass: 'border-emerald-300 bg-emerald-50/75 text-emerald-900 dark:border-emerald-700 dark:bg-emerald-900/25 dark:text-emerald-100', iconClass: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/60 dark:text-emerald-200' },
]
const selectedOption = computed(() => options.find(option => option.value === props.modelValue) ?? options[0])
</script>
