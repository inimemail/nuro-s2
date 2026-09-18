<template>
  <section class="space-y-4 rounded-xl border border-sky-200 bg-sky-50/50 p-4 dark:border-sky-800/50 dark:bg-sky-950/20">
    <div>
      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">OpenCode</h3>
      <div class="mt-3 grid grid-cols-2 gap-3">
        <button v-for="option in modes" :key="option.value" type="button"
          :aria-pressed="mode === option.value" @click="$emit('update:mode', option.value)"
          class="rounded-lg border p-3 text-left transition focus-visible:outline focus-visible:outline-2 focus-visible:outline-sky-500"
          :class="mode === option.value ? 'border-sky-500 bg-white shadow-sm dark:bg-dark-800' : 'border-gray-200 hover:border-sky-300 dark:border-dark-600'">
          <span class="block text-sm font-medium text-gray-900 dark:text-gray-100">{{ option.label }}</span>
          <span class="mt-1 block text-xs leading-5 text-gray-500 dark:text-gray-400">{{ option.description }}</span>
        </button>
      </div>
    </div>
    <div v-if="adaptive" class="space-y-3 border-t border-sky-100 pt-4 dark:border-sky-900">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <label class="text-sm font-medium text-gray-800 dark:text-gray-200">{{ t('admin.accounts.openCode.rules') }}</label>
        <button type="button" class="text-xs font-medium text-sky-600 hover:underline dark:text-sky-400" @click="$emit('update:rules', rules === null ? defaultOpenCodeRules(mode) : null)">
          {{ rules === null ? t('admin.accounts.openCode.customize') : t('admin.accounts.openCode.restore') }}
        </button>
      </div>
      <p class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openCode.rulesHint') }}</p>
      <div v-for="(rule, index) in displayedRules" :key="index" class="flex flex-wrap items-center gap-2 rounded-lg bg-white/80 p-2 dark:bg-dark-800/70">
        <input :value="rule.pattern" :disabled="rules === null" :aria-label="t('admin.accounts.openCode.pattern')" maxlength="128"
          class="input min-w-0 flex-1 font-mono text-xs" placeholder="gpt-*" @input="updateRule(index, 'pattern', ($event.target as HTMLInputElement).value)" />
        <select :value="rule.protocol" :disabled="rules === null" :aria-label="t('admin.accounts.cnProviders.apiProtocol')"
          class="input w-auto text-xs" @change="updateRule(index, 'protocol', ($event.target as HTMLSelectElement).value)">
          <option value="responses">Responses</option><option value="anthropic">Anthropic</option><option value="chat_completions">Chat Completions</option>
        </select>
        <button v-if="rules !== null" type="button" class="rounded px-2 py-1 text-xs text-red-500 hover:bg-red-50 dark:hover:bg-red-950" :aria-label="t('common.delete')" @click="$emit('update:rules', rules.filter((_, i) => i !== index))">×</button>
      </div>
      <button v-if="rules !== null && rules.length < 64" type="button" class="text-xs font-medium text-sky-600 hover:underline dark:text-sky-400" @click="$emit('update:rules', [...rules, { pattern: '', protocol: 'chat_completions' }])">+ {{ t('admin.accounts.openCode.addRule') }}</button>
      <p v-if="!validOpenCodeRules(rules)" role="alert" class="text-xs text-red-600 dark:text-red-400">{{ t('admin.accounts.openCode.invalidRules') }}</p>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { defaultOpenCodeRules, validOpenCodeRules, type OpenCodeMode, type OpenCodeRule } from '@/utils/opencode'
const props = defineProps<{ mode: OpenCodeMode; rules: OpenCodeRule[] | null; adaptive: boolean }>()
const emit = defineEmits<{ 'update:mode': [OpenCodeMode]; 'update:rules': [OpenCodeRule[] | null] }>()
const { t } = useI18n()
const modes = computed(() => [
  { value: 'go' as const, label: 'Go', description: t('admin.accounts.openCode.goDesc') },
  { value: 'zen' as const, label: 'Zen', description: t('admin.accounts.openCode.zenDesc') }
])
const displayedRules = computed(() => props.rules ?? defaultOpenCodeRules(props.mode))
function updateRule(index: number, key: keyof OpenCodeRule, value: string) {
  if (props.rules === null) return
  emit('update:rules', props.rules.map((rule, i) => i === index ? { ...rule, [key]: value } as OpenCodeRule : rule))
}
</script>
