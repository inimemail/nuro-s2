<template>
  <div class="mt-3 rounded-xl border border-rose-200 bg-gradient-to-br from-white via-rose-50/40 to-orange-50/50 p-4 shadow-sm dark:border-rose-900/60 dark:from-dark-800 dark:via-rose-950/20 dark:to-orange-950/20">
    <div class="flex items-start gap-3">
      <span class="inline-flex h-8 w-8 flex-shrink-0 items-center justify-center rounded-lg bg-rose-100 text-rose-600 dark:bg-rose-900/40 dark:text-rose-300" aria-hidden="true">
        <Icon name="bolt" size="sm" :stroke-width="2" />
      </span>
      <div class="min-w-0">
        <label class="input-label mb-0">{{ t('admin.accounts.upstreamConcurrencyRaceRules') }}</label>
        <p class="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.upstreamConcurrencyRaceRulesHint') }}
        </p>
      </div>
    </div>

    <div class="mt-4 overflow-hidden rounded-lg border border-gray-200/80 bg-white/85 dark:border-dark-600 dark:bg-dark-700/55">
      <div class="grid grid-cols-[minmax(0,1fr)_7rem_2rem] items-center gap-2 border-b border-gray-200/80 bg-gray-50/80 px-3 py-2 text-xs font-medium text-gray-600 dark:border-dark-600 dark:bg-dark-800/50 dark:text-gray-300">
        <span>{{ t('admin.accounts.upstreamConcurrencyRaceRuleMatcher') }}</span>
        <span>{{ t('admin.accounts.upstreamConcurrencyRaceRuleLimit') }}</span>
        <span class="sr-only">{{ t('admin.accounts.poolModeRetryRemoveStatusCode', { code: '' }) }}</span>
      </div>
      <div v-for="rule in normalizedRules" :key="rule.matcher" class="grid grid-cols-[minmax(0,1fr)_7rem_2rem] items-center gap-2 border-b border-gray-100 px-3 py-2 last:border-b-0 dark:border-dark-600/70">
        <div class="min-w-0">
          <span class="font-semibold tabular-nums text-gray-800 dark:text-gray-100">{{ rule.matcher }}</span>
          <span v-if="ruleLabel(rule.matcher)" class="ml-2 text-xs text-gray-500 dark:text-gray-400">{{ ruleLabel(rule.matcher) }}</span>
        </div>
        <input
          :value="rule.max_retries"
          type="number"
          min="0"
          max="200"
          step="1"
          class="input h-8 px-2 text-sm tabular-nums"
          :data-testid="`race-retry-limit-${rule.matcher}`"
          :aria-invalid="inputError ? 'true' : 'false'"
          @input="updateLimit(rule.matcher, $event)"
        />
        <button
          type="button"
          class="inline-flex h-8 w-8 items-center justify-center rounded-md text-gray-400 transition-colors hover:bg-rose-50 hover:text-rose-600 focus:outline-none focus:ring-2 focus:ring-rose-500 dark:hover:bg-rose-900/30 dark:hover:text-rose-300"
          :aria-label="t('admin.accounts.upstreamConcurrencyRaceRemoveRule', { matcher: rule.matcher })"
          :data-testid="`race-retry-remove-${rule.matcher}`"
          @click="removeRule(rule.matcher)"
        >
          <Icon name="x" size="xs" :stroke-width="2" />
        </button>
      </div>
      <div v-if="normalizedRules.length === 0" class="px-3 py-4 text-center text-xs text-gray-400">{{ t('admin.accounts.upstreamConcurrencyRaceNoRules') }}</div>
    </div>

    <div class="mt-3 flex gap-2">
      <input v-model="matcherInput" type="text" inputmode="text" maxlength="4" class="input h-9 min-w-0 flex-1 text-sm tabular-nums" :placeholder="t('admin.accounts.upstreamConcurrencyRaceRulePlaceholder')" data-testid="race-retry-rule-input" @keydown.enter.prevent="addRule" />
      <button type="button" class="inline-flex h-9 flex-shrink-0 items-center justify-center gap-1.5 rounded-lg bg-rose-600 px-3.5 text-sm font-medium text-white shadow-sm transition-all hover:bg-rose-700 focus:outline-none focus:ring-2 focus:ring-rose-500 focus:ring-offset-2 dark:focus:ring-offset-dark-800" data-testid="race-retry-add-rule" @click="addRule">
        <Icon name="plus" size="sm" :stroke-width="2" />
        {{ t('admin.accounts.upstreamConcurrencyRaceAddRule') }}
      </button>
    </div>
    <p v-if="inputError" class="mt-1.5 text-xs text-red-500" data-testid="race-retry-rule-error">{{ inputError }}</p>

    <div class="mt-3 rounded-lg border border-gray-200/80 bg-white/80 p-3 dark:border-dark-600 dark:bg-dark-700/55">
      <div class="flex items-start justify-between gap-4">
        <div class="min-w-0">
          <div class="text-sm font-medium text-gray-800 dark:text-gray-200">{{ t('admin.accounts.upstreamConcurrencyRaceTransport') }}</div>
          <p class="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('admin.accounts.upstreamConcurrencyRaceTransportHint') }}</p>
        </div>
        <button type="button" role="switch" :aria-checked="transportEnabled" :aria-label="t('admin.accounts.upstreamConcurrencyRaceTransport')" :class="['relative mt-0.5 inline-flex h-6 w-11 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors focus:outline-none focus:ring-2 focus:ring-rose-500 focus:ring-offset-2 dark:focus:ring-offset-dark-800', transportEnabled ? 'bg-rose-600' : 'bg-gray-200 dark:bg-dark-600']" data-testid="race-retry-transport-toggle" @click="emit('update:transportEnabled', !transportEnabled)">
          <span :class="['pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow transition duration-200', transportEnabled ? 'translate-x-5' : 'translate-x-0']" />
        </button>
      </div>
      <div v-if="transportEnabled" class="mt-3 flex items-center justify-between gap-3">
        <label class="text-xs text-gray-600 dark:text-gray-300">{{ t('admin.accounts.upstreamConcurrencyRaceTransportCount') }}</label>
        <input
          :value="transportRetryCount"
          type="number"
          min="0"
          :max="totalLimit"
          step="1"
          class="input h-8 w-28 px-2 text-sm tabular-nums"
          data-testid="race-retry-transport-count"
          :aria-invalid="transportOverBudget || transportError ? 'true' : 'false'"
          @input="updateTransportCount($event)"
        />
      </div>
      <p v-if="transportError || transportOverBudget" class="mt-2 text-xs text-red-500" data-testid="race-retry-transport-error">
        {{ transportError || t('admin.accounts.upstreamConcurrencyRaceTransportBudgetExceeded') }}
      </p>
    </div>

    <div class="mt-3 flex flex-wrap items-center justify-between gap-2 text-xs" :class="remaining < 0 ? 'text-red-600 dark:text-red-300' : 'text-gray-500 dark:text-gray-400'">
      <span>{{ t('admin.accounts.upstreamConcurrencyRaceRuleBudget', { used, total }) }}</span>
      <span v-if="remaining < 0">{{ t('admin.accounts.upstreamConcurrencyRaceRuleBudgetExceeded') }}</span>
      <span v-else-if="remaining === 0">{{ t('admin.accounts.upstreamConcurrencyRaceRuleBudgetFull') }}</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

export interface PoolModeRaceRetryRule { matcher: string; max_retries: number }

const props = withDefaults(defineProps<{
  modelValue: PoolModeRaceRetryRule[]
  total: number
  transportEnabled?: boolean
  transportRetryCount?: number
}>(), { transportEnabled: true, transportRetryCount: 1 })
const emit = defineEmits<{
  'update:modelValue': [value: PoolModeRaceRetryRule[]]
  'update:transportEnabled': [value: boolean]
  'update:transportRetryCount': [value: number]
}>()
const { t } = useI18n()
const matcherInput = ref('')
const inputError = ref('')
const transportError = ref('')
const normalizedRules = computed(() => {
  const seen = new Set<string>()
  return (Array.isArray(props.modelValue) ? props.modelValue : []).filter(rule => {
    const matcher = String(rule?.matcher ?? '').trim().toLowerCase()
    if (!matcher || seen.has(matcher)) return false
    seen.add(matcher)
    return true
  }).map(rule => ({ matcher: String(rule.matcher).trim().toLowerCase(), max_retries: Math.max(0, Math.min(200, Math.trunc(Number(rule.max_retries) || 0))) }))
})
const totalLimit = computed(() => Math.max(0, Math.min(200, Math.trunc(Number(props.total) || 0))))
const used = computed(() => normalizedRules.value.reduce((sum, rule) => sum + rule.max_retries, 0))
const remaining = computed(() => totalLimit.value - used.value)
const transportOverBudget = computed(() => props.transportEnabled && Math.trunc(Number(props.transportRetryCount) || 0) > totalLimit.value)
const labels: Record<string, string> = {
  '401': 'admin.accounts.poolModeRetryCode401', '403': 'admin.accounts.poolModeRetryCode403', '408': 'admin.accounts.poolModeRetryCode408',
  '429': 'admin.accounts.poolModeRetryCode429', '5xx': 'admin.accounts.upstreamConcurrencyRaceRule5xx', '502': 'admin.accounts.poolModeRetryCode502', '503': 'admin.accounts.poolModeRetryCode503'
}
function ruleLabel(matcher: string) { const key = labels[matcher]; return key ? t(key) : '' }
function emitRules(next: PoolModeRaceRetryRule[]) { emit('update:modelValue', next.map(rule => ({ matcher: rule.matcher, max_retries: Math.max(0, Math.min(200, Math.trunc(rule.max_retries))) }))) }
function addRule() {
  inputError.value = ''
  const matcher = matcherInput.value.trim().toLowerCase()
  if (!/^\d{3}$/.test(matcher) && matcher !== '5xx') { inputError.value = t('admin.accounts.upstreamConcurrencyRaceRuleInvalid'); return }
  if (matcher !== '5xx' && (Number(matcher) < 100 || Number(matcher) > 599)) { inputError.value = t('admin.accounts.upstreamConcurrencyRaceRuleInvalid'); return }
  if (normalizedRules.value.some(rule => rule.matcher === matcher)) { inputError.value = t('admin.accounts.upstreamConcurrencyRaceRuleExists'); return }
  emitRules([...normalizedRules.value, { matcher, max_retries: 0 }])
  matcherInput.value = ''
}
function removeRule(matcher: string) { inputError.value = ''; emitRules(normalizedRules.value.filter(rule => rule.matcher !== matcher)) }
function updateLimit(matcher: string, event: Event) {
  const target = event.target as HTMLInputElement
  const next = Math.max(0, Math.min(200, Math.trunc(Number(target.value) || 0)))
  const current = normalizedRules.value.find(rule => rule.matcher === matcher)?.max_retries ?? 0
  const other = used.value - current
  inputError.value = ''
  if (other + next > totalLimit.value) {
    target.value = String(current)
    inputError.value = t('admin.accounts.upstreamConcurrencyRaceRuleBudgetExceeded')
    return
  }
  emitRules(normalizedRules.value.map(rule => rule.matcher === matcher ? { ...rule, max_retries: next } : rule))
}
function updateTransportCount(event: Event) {
  const target = event.target as HTMLInputElement
  const current = Math.max(0, Math.min(200, Math.trunc(Number(props.transportRetryCount) || 0)))
  const next = Math.max(0, Math.min(200, Math.trunc(Number(target.value) || 0)))
  transportError.value = ''
  if (next > totalLimit.value) {
    target.value = String(current)
    transportError.value = t('admin.accounts.upstreamConcurrencyRaceTransportBudgetExceeded')
    return
  }
  emit('update:transportRetryCount', next)
}
watch(matcherInput, () => { if (inputError.value) inputError.value = '' })
watch(() => props.total, () => { transportError.value = '' })
</script>
