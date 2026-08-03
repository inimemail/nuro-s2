<template>
  <section v-if="!guardEnabled" class="mt-3 max-w-xs">
    <label class="input-label">{{ t('admin.accounts.openai.firstTokenTimeoutPlaceholderMs') }}</label>
    <div class="relative">
      <input
        :value="stages[0]?.placeholder_ms"
        type="number"
        inputmode="numeric"
        step="1"
        min="1"
        max="3000"
        class="input pr-12 font-mono"
        data-testid="single-placeholder"
        @input="updateStage(0, 'placeholder_ms', $event)"
      />
      <span class="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs font-medium text-gray-400">ms</span>
    </div>
    <p class="input-hint">{{ t('admin.accounts.openai.firstTokenTimeoutPlaceholderMsHint') }}</p>
  </section>
  <section v-else class="mt-4 border-t border-amber-200/80 pt-4 dark:border-amber-800/70">
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div>
        <div class="flex flex-wrap items-center gap-2">
          <h4 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.accounts.openai.firstTokenTimeoutStages.title') }}</h4>
          <span class="rounded bg-emerald-50 px-1.5 py-0.5 text-[11px] font-medium text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300">
            {{ t('admin.accounts.openai.firstTokenTimeoutStages.automatic') }}
          </span>
        </div>
        <p class="mt-1 max-w-2xl text-xs leading-5 text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.openai.firstTokenTimeoutStages.description') }}
        </p>
      </div>
      <button
        type="button"
        class="inline-flex items-center gap-1.5 rounded-md border border-primary-300 px-2.5 py-1.5 text-xs font-medium text-primary-700 transition hover:bg-primary-50 disabled:cursor-not-allowed disabled:opacity-40 dark:border-primary-700 dark:text-primary-300 dark:hover:bg-primary-900/20"
        :disabled="!canAddStage"
        data-testid="add-first-token-stage"
        @click="addStage"
      >
        <Icon name="plus" size="xs" />
        {{ t('admin.accounts.openai.firstTokenTimeoutStages.add') }}
      </button>
    </div>

    <div class="mt-3 space-y-2">
      <div
        v-for="(stage, index) in stages"
        :key="stage.stage"
        class="rounded-md border bg-white p-3 transition dark:bg-dark-700"
        :class="stageErrors[index].length ? 'border-red-300 dark:border-red-700' : 'border-gray-200 dark:border-dark-600'"
      >
        <div class="flex flex-wrap items-center gap-3">
          <div class="inline-flex min-w-[88px] items-center gap-2 text-sm font-medium text-gray-800 dark:text-gray-100">
            <span class="flex h-5 w-5 items-center justify-center rounded bg-gray-100 text-[11px] font-semibold text-gray-600 dark:bg-dark-600 dark:text-gray-300">{{ stage.stage }}</span>
            {{ t('admin.accounts.openai.firstTokenTimeoutStages.stage', { stage: stage.stage }) }}
          </div>
          <label class="flex min-w-[150px] flex-1 items-center gap-2 text-xs text-gray-600 dark:text-gray-300">
            <span class="whitespace-nowrap">{{ t('admin.accounts.openai.firstTokenTimeoutStages.placeholder') }}</span>
            <input
              :value="stage.placeholder_ms"
              type="number"
              min="1"
              max="3000"
              class="input h-8 min-w-0 flex-1 text-right text-sm"
              :data-testid="`stage-${stage.stage}-placeholder`"
              @input="updateStage(index, 'placeholder_ms', $event)"
            />
            <span>ms</span>
          </label>
          <label class="flex min-w-[170px] flex-1 items-center gap-2 text-xs text-gray-600 dark:text-gray-300">
            <span class="whitespace-nowrap">{{ t('admin.accounts.openai.firstTokenTimeoutStages.guard') }}</span>
            <input
              :value="stage.guard_max_ms"
              type="number"
              min="1"
              max="30000"
              class="input h-8 min-w-0 flex-1 text-right text-sm"
              :data-testid="`stage-${stage.stage}-guard`"
              @input="updateStage(index, 'guard_max_ms', $event)"
            />
            <span>ms</span>
          </label>
          <button
            v-if="index > 0"
            type="button"
            class="inline-flex h-8 w-8 items-center justify-center rounded-md text-gray-400 transition hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20"
            :title="t('admin.accounts.openai.firstTokenTimeoutStages.remove')"
            :data-testid="`remove-stage-${stage.stage}`"
            @click="removeStage(index)"
          >
            <Icon name="trash" size="sm" />
          </button>
        </div>
        <p v-for="error in stageErrors[index]" :key="error" class="mt-1.5 text-xs text-red-600 dark:text-red-400">
          {{ error }}
        </p>
      </div>
    </div>
    <div v-if="firstInvalidIndex >= 0" class="mt-3 flex flex-wrap items-center gap-2 border-t border-red-200 pt-3 dark:border-red-900/60">
      <span class="mr-auto text-xs text-red-600 dark:text-red-400">{{ t('admin.accounts.openai.firstTokenTimeoutStages.conflict', { stage: firstInvalidIndex + 1 }) }}</span>
      <button type="button" class="rounded-md border border-gray-300 px-2.5 py-1.5 text-xs font-medium text-gray-700 hover:bg-gray-100 dark:border-dark-500 dark:text-gray-200 dark:hover:bg-dark-600" @click="repairFollowingStages">
        {{ t('admin.accounts.openai.firstTokenTimeoutStages.repair') }}
      </button>
      <button v-if="firstInvalidIndex > 0" type="button" class="rounded-md border border-red-300 px-2.5 py-1.5 text-xs font-medium text-red-700 hover:bg-red-50 dark:border-red-800 dark:text-red-300 dark:hover:bg-red-900/20" @click="removeInvalidTail">
        {{ t('admin.accounts.openai.firstTokenTimeoutStages.removeInvalid') }}
      </button>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import type { OpenAIApiKeyFirstTokenTimeoutStageConfig } from '@/utils/openaiFirstTokenTimeoutStages'

const props = withDefaults(defineProps<{
  modelValue: OpenAIApiKeyFirstTokenTimeoutStageConfig
  guardEnabled?: boolean
}>(), { guardEnabled: true })
const emit = defineEmits<{ 'update:modelValue': [value: OpenAIApiKeyFirstTokenTimeoutStageConfig] }>()
const { t } = useI18n()

const stages = computed(() => props.modelValue.stages)

const stageErrors = computed(() => stages.value.map((stage, index) => {
  const errors: string[] = []
  if (!Number.isInteger(stage.placeholder_ms) || stage.placeholder_ms < 1 || stage.placeholder_ms > 3000) errors.push(t('admin.accounts.openai.firstTokenTimeoutStages.placeholderRange'))
  if (!Number.isInteger(stage.guard_max_ms) || stage.guard_max_ms < 1 || stage.guard_max_ms > 30000) errors.push(t('admin.accounts.openai.firstTokenTimeoutStages.guardRange'))
  if (stage.guard_max_ms < stage.placeholder_ms) errors.push(t('admin.accounts.openai.firstTokenTimeoutStages.guardBelowPlaceholder'))
  if (index > 0) {
    const previousStages = stages.value.slice(0, index)
    const placeholderReference = previousStages.reduce((best, item) => item.placeholder_ms > best.placeholder_ms ? item : best)
    const guardReference = previousStages.reduce((best, item) => item.guard_max_ms > best.guard_max_ms ? item : best)
    if (stage.placeholder_ms <= placeholderReference.placeholder_ms) errors.push(t('admin.accounts.openai.firstTokenTimeoutStages.placeholderOrder', { stage: placeholderReference.stage }))
    if (stage.guard_max_ms <= guardReference.guard_max_ms) errors.push(t('admin.accounts.openai.firstTokenTimeoutStages.guardOrder', { stage: guardReference.stage }))
  }
  return errors
}))
const firstInvalidIndex = computed(() => stageErrors.value.findIndex((errors) => errors.length > 0))
const canAddStage = computed(() => {
  if (stages.value.length >= 10 || firstInvalidIndex.value >= 0) return false
  const previous = stages.value[stages.value.length - 1]
  return previous.placeholder_ms < 3000 && previous.guard_max_ms < 30000
})

function updateStage(index: number, field: 'placeholder_ms' | 'guard_max_ms', event: Event) {
  const value = Number((event.target as HTMLInputElement).value)
  emit('update:modelValue', {
    stages: stages.value.map((stage, itemIndex) => itemIndex === index ? { ...stage, [field]: value } : { ...stage })
  })
}

function addStage() {
  if (stages.value.length >= 10) return
  const previous = stages.value[stages.value.length - 1]
  const placeholder = Math.min(3000, Math.max(previous.placeholder_ms + 1, previous.placeholder_ms * 1.25))
  const guard = Math.min(30000, Math.max(previous.guard_max_ms + 1, previous.guard_max_ms * 1.25))
  emit('update:modelValue', {
    stages: [...stages.value, { stage: stages.value.length + 1, placeholder_ms: Math.round(placeholder), guard_max_ms: Math.round(guard) }]
  })
}

function removeStage(index: number) {
  if (index <= 0) return
  const next = stages.value.filter((_, itemIndex) => itemIndex !== index).map((stage, itemIndex) => ({ ...stage, stage: itemIndex + 1 }))
  emit('update:modelValue', { stages: next })
}

function repairFollowingStages() {
  const repaired = stages.value.map((stage) => ({ ...stage }))
  for (let index = 0; index < repaired.length; index += 1) {
    const previous = repaired[index - 1]
    repaired[index].placeholder_ms = Math.min(3000, Math.max(1, Math.round(repaired[index].placeholder_ms), previous ? previous.placeholder_ms + 1 : 1))
    repaired[index].guard_max_ms = Math.min(30000, Math.max(repaired[index].placeholder_ms, Math.round(repaired[index].guard_max_ms), previous ? previous.guard_max_ms + 1 : 1))
  }
  emit('update:modelValue', { stages: repaired })
}

function removeInvalidTail() {
  if (firstInvalidIndex.value <= 0) return
  const next = stages.value.slice(0, firstInvalidIndex.value).map((stage, index) => ({ ...stage, stage: index + 1 }))
  emit('update:modelValue', { stages: next })
}
</script>
