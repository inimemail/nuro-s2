<template>
  <section class="rounded-xl border border-gray-200 bg-white shadow-sm dark:border-dark-600 dark:bg-dark-800">
    <div class="flex flex-wrap items-start justify-between gap-4 border-b border-gray-100 px-5 py-4 dark:border-dark-700">
      <div>
        <div class="flex items-center gap-2">
          <span class="flex h-8 w-8 items-center justify-center rounded-lg bg-primary-50 text-primary-600 dark:bg-primary-900/30 dark:text-primary-300">
            <Icon name="clock" size="sm" />
          </span>
          <h3 class="text-base font-semibold text-gray-900 dark:text-white">{{ title }}</h3>
        </div>
        <p class="mt-2 max-w-2xl text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('admin.settings.gatewayFirstTokenDefaults.description') }}</p>
      </div>
      <button
        type="button"
        class="inline-flex items-center gap-1.5 rounded-md border border-gray-300 px-3 py-1.5 text-xs font-medium text-gray-700 transition hover:bg-gray-50 dark:border-dark-500 dark:text-gray-200 dark:hover:bg-dark-700"
        @click="restoreDefaults"
      >
        <Icon name="refresh" size="xs" />
        {{ t('admin.settings.gatewayFirstTokenDefaults.restore') }}
      </button>
    </div>

    <div class="space-y-3 p-5">
      <div class="grid grid-cols-[40px_minmax(0,1fr)_minmax(0,1fr)_36px] items-center gap-3 px-1 text-[11px] font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
        <span>#</span>
        <span>{{ t('admin.settings.gatewayFirstTokenDefaults.placeholder') }}</span>
        <span>{{ t('admin.settings.gatewayFirstTokenDefaults.guard') }}</span>
        <span class="sr-only">{{ t('admin.settings.gatewayFirstTokenDefaults.remove') }}</span>
      </div>
      <div
        v-for="(stage, index) in stages"
        :key="stage.stage"
        class="grid grid-cols-[40px_minmax(0,1fr)_minmax(0,1fr)_36px] items-center gap-3 rounded-lg border px-3 py-2.5 transition"
        :class="rowErrors[index] ? 'border-red-300 bg-red-50/40 dark:border-red-800 dark:bg-red-950/20' : 'border-gray-200 bg-gray-50/50 dark:border-dark-600 dark:bg-dark-700/40'"
      >
        <span class="flex h-7 w-7 items-center justify-center rounded-md bg-white text-xs font-semibold text-gray-600 shadow-sm dark:bg-dark-600 dark:text-gray-200">{{ index + 1 }}</span>
        <label class="relative">
          <span class="sr-only">{{ t('admin.settings.gatewayFirstTokenDefaults.placeholder') }}</span>
          <input v-model.number="stage.placeholder_ms" type="number" min="1" max="100000" step="1" class="input h-9 w-full pr-10 text-right font-mono text-sm" />
          <span class="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-gray-400">ms</span>
        </label>
        <label class="relative">
          <span class="sr-only">{{ t('admin.settings.gatewayFirstTokenDefaults.guard') }}</span>
          <input v-model.number="stage.guard_max_ms" type="number" min="1" step="1" class="input h-9 w-full pr-10 text-right font-mono text-sm" />
          <span class="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-gray-400">ms</span>
        </label>
        <button
          type="button"
          class="inline-flex h-8 w-8 items-center justify-center rounded-md text-gray-400 transition hover:bg-red-50 hover:text-red-600 disabled:cursor-not-allowed disabled:opacity-30 dark:hover:bg-red-950/30"
          :disabled="index === 0"
          :title="t('admin.settings.gatewayFirstTokenDefaults.remove')"
          @click="removeStage(index)"
        >
          <Icon name="trash" size="sm" />
        </button>
        <p v-if="rowErrors[index]" class="col-span-full -mt-1 text-xs text-red-600 dark:text-red-400">{{ rowErrors[index] }}</p>
      </div>

      <div class="flex flex-wrap items-center justify-between gap-3 border-t border-gray-100 pt-3 dark:border-dark-700">
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.settings.gatewayFirstTokenDefaults.footer', { count: stages.length }) }}</p>
        <button
          type="button"
          class="inline-flex items-center gap-1.5 rounded-md border border-primary-300 px-3 py-1.5 text-xs font-medium text-primary-700 transition hover:bg-primary-50 disabled:cursor-not-allowed disabled:opacity-40 dark:border-primary-700 dark:text-primary-300 dark:hover:bg-primary-900/20"
          :disabled="stages.length >= 10"
          @click="addStage"
        >
          <Icon name="plus" size="xs" />
          {{ t('admin.settings.gatewayFirstTokenDefaults.add') }}
        </button>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import type { OpenAIFirstTokenTimeoutPlaceholderStage } from '@/api/admin/settings'

const props = defineProps<{
  modelValue: OpenAIFirstTokenTimeoutPlaceholderStage[]
  title: string
}>()
const emit = defineEmits<{ 'update:modelValue': [value: OpenAIFirstTokenTimeoutPlaceholderStage[]] }>()
const { t } = useI18n()

const stages = computed(() => props.modelValue)
const rowErrors = computed(() => stages.value.map((stage, index) => {
  if (!Number.isInteger(stage.placeholder_ms) || stage.placeholder_ms < 1 || stage.placeholder_ms > 100000) return t('admin.settings.gatewayFirstTokenDefaults.invalidPlaceholder')
  if (!Number.isInteger(stage.guard_max_ms) || stage.guard_max_ms < stage.placeholder_ms) return t('admin.settings.gatewayFirstTokenDefaults.invalidGuard')
  const previous = stages.value[index - 1]
  if (previous && (stage.placeholder_ms <= previous.placeholder_ms || stage.guard_max_ms <= previous.guard_max_ms)) return t('admin.settings.gatewayFirstTokenDefaults.invalidOrder')
  return ''
}))

const builtInDefaults = (): OpenAIFirstTokenTimeoutPlaceholderStage[] => [
  { stage: 1, placeholder_ms: 800, guard_max_ms: 5000 },
  { stage: 2, placeholder_ms: 3000, guard_max_ms: 10000 },
  { stage: 3, placeholder_ms: 5000, guard_max_ms: 15000 },
  { stage: 4, placeholder_ms: 10000, guard_max_ms: 30000 },
]

function restoreDefaults() { emit('update:modelValue', builtInDefaults()) }
function addStage() {
  if (stages.value.length >= 10) return
  const previous = stages.value[stages.value.length - 1] || { placeholder_ms: 800, guard_max_ms: 5000 }
  emit('update:modelValue', [...stages.value, { stage: stages.value.length + 1, placeholder_ms: previous.placeholder_ms + 1000, guard_max_ms: previous.guard_max_ms + 5000 }])
}
function removeStage(index: number) {
  if (index === 0) return
  emit('update:modelValue', stages.value.filter((_, itemIndex) => itemIndex !== index).map((stage, itemIndex) => ({ ...stage, stage: itemIndex + 1 })))
}
</script>
