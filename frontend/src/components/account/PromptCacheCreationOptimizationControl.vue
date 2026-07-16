<template>
  <div class="rounded-lg bg-sky-50 p-3 dark:bg-sky-900/15">
    <div class="flex items-center justify-between gap-4">
      <div class="min-w-0">
        <label class="input-label mb-0">{{ t('admin.accounts.promptCacheCreationOptimization') }}</label>
        <p class="mt-1 text-xs text-sky-700 dark:text-sky-300">
          {{ t('admin.accounts.promptCacheCreationOptimizationHint') }}
        </p>
      </div>
      <button
        type="button"
        data-testid="prompt-cache-creation-optimization-toggle"
        :aria-label="t('admin.accounts.promptCacheCreationOptimization')"
        :aria-pressed="enabled"
        @click="emit('update:enabled', !enabled)"
        :class="[
          'relative inline-flex h-6 w-11 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none focus:ring-2 focus:ring-sky-500 focus:ring-offset-2',
          enabled ? 'bg-sky-600' : 'bg-gray-200 dark:bg-dark-600'
        ]"
      >
        <span
          :class="[
            'pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out',
            enabled ? 'translate-x-5' : 'translate-x-0'
          ]"
        />
      </button>
    </div>

    <div v-if="enabled" class="mt-3">
      <div class="grid grid-cols-2 gap-1 rounded-lg bg-sky-100/80 p-1 dark:bg-dark-800/70">
        <button
          v-for="option in modeOptions"
          :key="option.value"
          type="button"
          :data-testid="`prompt-cache-creation-mode-${option.value}`"
          :aria-pressed="mode === option.value"
          @click="emit('update:mode', option.value)"
          :class="[
            'min-w-0 rounded-md px-2 py-2 text-center text-xs font-medium transition-colors',
            mode === option.value
              ? 'bg-white text-sky-800 shadow-sm dark:bg-dark-700 dark:text-sky-200'
              : 'text-gray-600 hover:text-sky-800 dark:text-gray-400 dark:hover:text-sky-200'
          ]"
        >
          {{ t(option.label) }}
        </button>
      </div>
      <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">
        {{ t(modeHintKey) }}
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

type PromptCacheCreationOptimizationMode = 'reduce' | 'suppress'

const props = defineProps<{
  enabled: boolean
  mode: PromptCacheCreationOptimizationMode
}>()

const emit = defineEmits<{
  'update:enabled': [value: boolean]
  'update:mode': [value: PromptCacheCreationOptimizationMode]
}>()

const { t } = useI18n()

const modeOptions: Array<{
  value: PromptCacheCreationOptimizationMode
  label: string
}> = [
  {
    value: 'reduce',
    label: 'admin.accounts.promptCacheCreationOptimizationReduce'
  },
  {
    value: 'suppress',
    label: 'admin.accounts.promptCacheCreationOptimizationSuppress'
  }
]

const modeHintKey = computed(() =>
  props.mode === 'suppress'
    ? 'admin.accounts.promptCacheCreationOptimizationSuppressHint'
    : 'admin.accounts.promptCacheCreationOptimizationReduceHint'
)
</script>
