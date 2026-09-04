<template>
  <div class="account-scheduling-field">
    <div class="mb-2 flex items-center justify-between gap-3">
      <label class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.groups.form.accountSchedulingStrategy') }}</label>
      <span class="text-[11px] font-medium uppercase tracking-wide text-gray-400 dark:text-gray-500">{{ t('admin.groups.form.strategyScope') }}</span>
    </div>
    <div class="grid grid-cols-2 gap-1 rounded-lg bg-gray-100 p-1 dark:bg-dark-700" role="group">
      <button
        v-for="option in options"
        :key="option.value"
        type="button"
        class="flex min-h-[42px] items-center gap-2 rounded-md px-3 text-left transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500/60"
        :class="modelValue === option.value ? 'bg-white text-primary-700 shadow-sm dark:bg-dark-800 dark:text-primary-300' : 'text-gray-600 hover:bg-white/70 hover:text-gray-900 dark:text-gray-300 dark:hover:bg-dark-600/80 dark:hover:text-white'"
        :aria-pressed="modelValue === option.value"
        @click="emit('update:modelValue', option.value)"
      >
        <span class="flex h-7 w-7 flex-none items-center justify-center rounded-md" :class="modelValue === option.value ? 'bg-primary-50 text-primary-600 dark:bg-primary-900/30 dark:text-primary-300' : 'bg-white/70 text-gray-400 dark:bg-dark-600 dark:text-gray-400'">
          <Icon :name="option.icon" size="sm" :stroke-width="2" />
        </span>
        <span class="min-w-0 truncate text-sm font-medium">{{ t(option.label) }}</span>
      </button>
    </div>
    <p class="mt-2 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t(selectedOption.hint) }}</p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

type Strategy = 'strict_priority' | 'health_first'
const props = withDefaults(defineProps<{ modelValue?: Strategy }>(), { modelValue: 'strict_priority' })
const emit = defineEmits<{ (event: 'update:modelValue', value: Strategy): void }>()
const { t } = useI18n()
const options = [
  { value: 'strict_priority' as const, icon: 'shield' as const, label: 'admin.groups.form.strictPriority', hint: 'admin.groups.form.strictPriorityHint' },
  { value: 'health_first' as const, icon: 'bolt' as const, label: 'admin.groups.form.healthFirst', hint: 'admin.groups.form.healthFirstHint' },
]
const selectedOption = computed(() => options.find(option => option.value === props.modelValue) ?? options[0])
</script>
