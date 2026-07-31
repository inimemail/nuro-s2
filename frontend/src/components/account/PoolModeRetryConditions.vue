<template>
  <div class="mt-3 rounded-xl border border-gray-200 bg-gradient-to-br from-white via-gray-50/90 to-primary-50/50 p-4 shadow-sm dark:border-dark-600 dark:from-dark-800 dark:via-dark-800/95 dark:to-primary-950/20">
    <div class="flex items-start gap-3">
      <span class="inline-flex h-8 w-8 flex-shrink-0 items-center justify-center rounded-lg bg-primary-100 text-primary-600 dark:bg-primary-900/40 dark:text-primary-300">
        <svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true">
          <path stroke-linecap="round" stroke-linejoin="round" d="M15.5 6.5A6.5 6.5 0 114.6 4.9M4.5 1.8v3.5H8" />
        </svg>
      </span>
      <div class="min-w-0">
        <label class="input-label mb-0">{{ t('admin.accounts.poolModeRetryConditions') }}</label>
        <p class="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.poolModeRetryConditionsHint') }}
        </p>
      </div>
    </div>

    <div class="mt-4 rounded-lg border border-gray-200/80 bg-white/80 p-3 dark:border-dark-600 dark:bg-dark-700/55">
      <div class="flex items-center justify-between gap-3">
        <span class="text-xs font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.accounts.poolModeRetryHttpStatusCodes') }}
        </span>
        <span class="text-[11px] text-gray-400 dark:text-gray-500">
          {{ t('admin.accounts.poolModeRetryStatusCodeRange') }}
        </span>
      </div>

      <div class="mt-2 flex min-h-9 flex-wrap items-center gap-2" data-testid="pool-retry-status-codes">
        <div
          v-for="code in normalizedCodes"
          :key="code"
          :data-testid="`pool-retry-status-code-${code}`"
          class="inline-flex items-center gap-1.5 rounded-lg border border-primary-200 bg-primary-50 px-2.5 py-1.5 text-xs text-primary-700 shadow-sm dark:border-primary-800 dark:bg-primary-900/20 dark:text-primary-300"
        >
          <span class="font-semibold tabular-nums">{{ code }}</span>
          <span v-if="statusCodeLabel(code)" class="text-primary-600/80 dark:text-primary-300/80">
            {{ statusCodeLabel(code) }}
          </span>
          <button
            type="button"
            :aria-label="t('admin.accounts.poolModeRetryRemoveStatusCode', { code })"
            class="ml-0.5 rounded p-0.5 text-primary-400 transition-colors hover:bg-primary-100 hover:text-primary-700 focus:outline-none focus:ring-2 focus:ring-primary-500 dark:hover:bg-primary-900/40 dark:hover:text-primary-200"
            :data-testid="`pool-retry-remove-status-code-${code}`"
            @click="removeStatusCode(code)"
          >
            <svg class="h-3.5 w-3.5" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
              <path d="M5.22 5.22a.75.75 0 011.06 0L10 8.94l3.72-3.72a.75.75 0 111.06 1.06L11.06 10l3.72 3.72a.75.75 0 11-1.06 1.06L10 11.06l-3.72 3.72a.75.75 0 11-1.06-1.06L8.94 10 5.22 6.28a.75.75 0 010-1.06z" />
            </svg>
          </button>
        </div>

        <span v-if="normalizedCodes.length === 0" class="text-xs text-gray-400 dark:text-gray-500">
          {{ t('admin.accounts.poolModeRetryNoStatusCodes') }}
        </span>
      </div>

      <div class="mt-3 flex gap-2">
        <input
          v-model="statusCodeInput"
          type="text"
          inputmode="numeric"
          maxlength="3"
          class="input h-9 min-w-0 flex-1 text-sm tabular-nums"
          :placeholder="t('admin.accounts.poolModeRetryStatusCodePlaceholder')"
          data-testid="pool-retry-status-code-input"
          @keydown.enter.prevent="addStatusCode"
        />
        <button
          type="button"
          class="inline-flex h-9 flex-shrink-0 items-center justify-center gap-1.5 rounded-lg bg-primary-600 px-3.5 text-sm font-medium text-white shadow-sm transition-all hover:bg-primary-700 hover:shadow focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-2 dark:focus:ring-offset-dark-800"
          data-testid="pool-retry-add-status-code"
          @click="addStatusCode"
        >
          <svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true">
            <path stroke-linecap="round" d="M10 4v12M4 10h12" />
          </svg>
          {{ t('admin.accounts.poolModeRetryAddStatusCode') }}
        </button>
      </div>
      <p v-if="inputError" class="mt-1.5 text-xs text-red-500" data-testid="pool-retry-status-code-error">
        {{ inputError }}
      </p>
    </div>

    <div v-if="showBuiltinTransient" class="mt-3 rounded-lg border border-gray-200/80 bg-white/80 p-3 dark:border-dark-600 dark:bg-dark-700/55">
      <div class="flex items-start justify-between gap-4">
        <div class="min-w-0">
          <div class="text-sm font-medium text-gray-800 dark:text-gray-200">
            {{ t('admin.accounts.poolModeBuiltinRetry') }}
          </div>
          <p class="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.poolModeBuiltinRetryHint') }}
          </p>
        </div>
        <button
          type="button"
          role="switch"
          :aria-checked="builtinTransientEnabled"
          :aria-label="t('admin.accounts.poolModeBuiltinRetry')"
          :class="[
            'relative mt-0.5 inline-flex h-6 w-11 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-2 dark:focus:ring-offset-dark-800',
            builtinTransientEnabled ? 'bg-primary-600' : 'bg-gray-200 dark:bg-dark-600'
          ]"
          data-testid="pool-mode-builtin-retry-toggle"
          @click="toggleBuiltinTransient"
        >
          <span
            :class="[
              'pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow ring-0 transition duration-200',
              builtinTransientEnabled ? 'translate-x-5' : 'translate-x-0'
            ]"
          />
        </button>
      </div>

      <div
        v-if="overlappingCodes.length > 0"
        class="mt-3 rounded-lg bg-amber-50 px-3 py-2 text-xs leading-5 text-amber-700 dark:bg-amber-900/20 dark:text-amber-300"
        data-testid="pool-retry-overlap-hint"
      >
        {{ t('admin.accounts.poolModeRetryOverlapHint', { codes: overlappingCodes.join(', ') }) }}
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

const props = withDefaults(defineProps<{
  modelValue: number[]
  builtinTransientEnabled?: boolean
  showBuiltinTransient?: boolean
}>(), {
  builtinTransientEnabled: true,
  showBuiltinTransient: false
})

const emit = defineEmits<{
  'update:modelValue': [value: number[]]
  'update:builtinTransientEnabled': [value: boolean]
}>()

const { t } = useI18n()
const statusCodeInput = ref('')
const inputError = ref('')

const normalizedCodes = computed(() => {
  const codes = Array.isArray(props.modelValue) ? props.modelValue : []
  return Array.from(new Set(codes.filter(code => Number.isInteger(code) && code >= 100 && code <= 599)))
    .sort((a, b) => a - b)
})

const overlappingCodes = computed(() => {
  if (!props.builtinTransientEnabled) return []
  return normalizedCodes.value.filter(code => code === 408 || code >= 500)
})

const statusCodeLabelKeys: Record<number, string> = {
  401: 'admin.accounts.poolModeRetryCode401',
  403: 'admin.accounts.poolModeRetryCode403',
  408: 'admin.accounts.poolModeRetryCode408',
  429: 'admin.accounts.poolModeRetryCode429',
  500: 'admin.accounts.poolModeRetryCode500',
  502: 'admin.accounts.poolModeRetryCode502',
  503: 'admin.accounts.poolModeRetryCode503',
  504: 'admin.accounts.poolModeRetryCode504',
  529: 'admin.accounts.poolModeRetryCode529'
}

function statusCodeLabel(code: number): string {
  const key = statusCodeLabelKeys[code]
  return key ? t(key) : ''
}

function addStatusCode() {
  inputError.value = ''
  const raw = statusCodeInput.value.trim()
  const code = Number(raw)
  if (!/^\d{3}$/.test(raw) || !Number.isInteger(code) || code < 100 || code > 599) {
    inputError.value = t('admin.accounts.invalidErrorCode')
    return
  }
  if (normalizedCodes.value.includes(code)) {
    inputError.value = t('admin.accounts.errorCodeExists')
    return
  }
  emit('update:modelValue', [...normalizedCodes.value, code].sort((a, b) => a - b))
  statusCodeInput.value = ''
}

function removeStatusCode(code: number) {
  inputError.value = ''
  emit('update:modelValue', normalizedCodes.value.filter(item => item !== code))
}

function toggleBuiltinTransient() {
  emit('update:builtinTransientEnabled', !props.builtinTransientEnabled)
}

watch(statusCodeInput, () => {
  if (inputError.value) inputError.value = ''
})
</script>
