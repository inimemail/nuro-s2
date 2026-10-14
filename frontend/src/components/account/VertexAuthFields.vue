<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useId } from 'vue'

defineProps<{ mode: 'service_account' | 'apikey'; apiKey: string; editing?: boolean }>()
const emit = defineEmits<{
  'update:mode': [value: 'service_account' | 'apikey']
  'update:apiKey': [value: string]
}>()
const { t } = useI18n()
const id = useId()
</script>

<template>
  <div class="rounded-xl border border-sky-200 bg-sky-50/60 p-4 dark:border-sky-900 dark:bg-sky-950/20">
    <label class="input-label" :for="`${id}-mode`">{{ t('admin.accounts.vertexAuthMode') }}</label>
    <select :id="`${id}-mode`" :value="mode" class="input" @change="emit('update:mode', ($event.target as HTMLSelectElement).value as 'service_account' | 'apikey')">
      <option value="service_account">Service Account JSON</option>
      <option value="apikey">Vertex API Key</option>
    </select>
    <div v-if="mode === 'apikey'" class="mt-3 space-y-2">
      <label class="input-label" :for="`${id}-key`">Vertex API Key</label>
      <input :id="`${id}-key`" :value="apiKey" type="password" autocomplete="new-password" class="input font-mono" :placeholder="editing ? t('admin.accounts.vertexKeyKeepHint') : 'AQ.Ab…'" @input="emit('update:apiKey', ($event.target as HTMLInputElement).value)" />
      <p class="text-xs leading-relaxed text-sky-800 dark:text-sky-300">{{ t('admin.accounts.vertexKeyModeHint') }}</p>
    </div>
  </div>
</template>
