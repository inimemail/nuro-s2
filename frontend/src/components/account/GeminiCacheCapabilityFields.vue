<script setup lang="ts">
import { useId } from 'vue'
import { useI18n } from 'vue-i18n'
defineProps<{ enabled: boolean; ttl: number; baseUrl: string; pool: boolean }>()
const emit = defineEmits<{ 'update:enabled': [value: boolean]; 'update:ttl': [value: number] }>()
const { t } = useI18n()
const id = useId()
function official(base: string) {
  if (!base.trim()) return true
  try { const url = new URL(base); return url.protocol === 'https:' && url.hostname === 'generativelanguage.googleapis.com' && !url.username && !url.password && (!url.port || url.port === '443') && url.pathname === '/' && !url.search && !url.hash && !base.includes('?') && !base.includes('#') } catch { return false }
}
</script>
<template>
  <div class="rounded-xl border border-teal-200 bg-teal-50/60 p-4 dark:border-teal-900 dark:bg-teal-950/20">
    <p class="text-sm font-semibold text-teal-900 dark:text-teal-200">{{ t('geminiCache.capability.title') }}</p>
    <p v-if="pool" class="mt-2 text-xs leading-6 text-teal-800 dark:text-teal-300">{{ t('geminiCache.capability.pool') }}</p>
    <p v-else-if="official(baseUrl)" class="mt-2 text-xs leading-6 text-teal-800 dark:text-teal-300">{{ t('geminiCache.capability.official') }}</p>
    <template v-else>
      <label :for="id" class="mt-3 flex cursor-pointer items-start gap-3"><input :id="id" type="checkbox" class="mt-1 rounded border-teal-300 text-teal-600 focus:ring-teal-500" :checked="enabled" @change="emit('update:enabled', ($event.target as HTMLInputElement).checked)" /><span class="text-sm leading-6">{{ t('geminiCache.capability.declare') }}</span></label>
      <p class="mt-2 text-xs leading-6 text-teal-800 dark:text-teal-300">{{ t('geminiCache.capability.hint') }}</p>
      <div v-if="enabled" class="mt-3"><label :for="`${id}-ttl`" class="input-label">{{ t('geminiCache.capability.ttl') }}</label><input :id="`${id}-ttl`" :value="ttl" type="number" min="60" max="86400" step="1" required class="input max-w-60" @input="emit('update:ttl', Number(($event.target as HTMLInputElement).value))" /></div>
    </template>
  </div>
</template>
