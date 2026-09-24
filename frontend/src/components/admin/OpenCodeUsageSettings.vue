<template>
  <section class="border-t border-gray-200 py-5 dark:border-dark-600">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('openCodeUsage.title') }}</h3>
        <p class="mt-1 max-w-2xl text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('openCodeUsage.description') }}</p>
      </div>
      <Toggle v-model="settings.enabled" :disabled="loading || !loaded" :aria-label="t('openCodeUsage.title')" />
    </div>
    <div v-if="settings.enabled" class="mt-4 grid gap-4 sm:grid-cols-2">
      <label class="text-xs font-medium text-gray-700 dark:text-gray-300">{{ t('openCodeUsage.interval') }}<input v-model.number="settings.interval_minutes" type="number" min="5" max="1440" step="1" class="input mt-2 w-full" /></label>
      <label class="text-xs font-medium text-gray-700 dark:text-gray-300">{{ t('openCodeUsage.debounce') }}<input v-model.number="settings.debounce_minutes" type="number" min="1" max="60" step="1" class="input mt-2 w-full" /></label>
    </div>
    <div class="mt-4 flex flex-wrap items-center justify-between gap-3 border-t border-gray-200 pt-3 dark:border-dark-600">
      <p class="text-xs text-gray-400">{{ saved ? t('openCodeUsage.saved') : t('openCodeUsage.manualAvailable') }}</p>
      <button type="button" class="btn btn-secondary !text-xs" :disabled="loading" @click="loaded ? save() : load()">{{ loaded ? t('common.save') : t('common.retry') }}</button>
    </div>
    <p v-if="error" class="mt-3 rounded-lg bg-red-50 p-3 text-xs text-red-600 dark:bg-red-900/20 dark:text-red-300" role="alert">{{ error }}</p>
  </section>
</template>
<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { openCodeUsageAPI, type OpenCodeUsageSettings } from '@/api/admin/opencodeUsage'
import { extractApiErrorMessage } from '@/utils/apiError'
const { t } = useI18n()
const settings = ref<OpenCodeUsageSettings>({ enabled: false, interval_minutes: 15, debounce_minutes: 1 })
const loading = ref(false)
const loaded = ref(false)
const saved = ref(false)
const error = ref('')
let generation = 0
async function load() {
  const version = ++generation
  loading.value = true
  error.value = ''
  try { const value = await openCodeUsageAPI.settings(); if (version === generation) { settings.value = value; loaded.value = true } }
  catch (err) { if (version === generation) error.value = extractApiErrorMessage(err, t('openCodeUsage.failed')) }
  finally { if (version === generation) loading.value = false }
}
async function save() {
  const { interval_minutes: interval, debounce_minutes: debounce } = settings.value
  if (!Number.isInteger(interval) || interval < 5 || interval > 1440 || !Number.isInteger(debounce) || debounce < 1 || debounce > 60) { error.value = t('openCodeUsage.invalid'); return }
  const version = ++generation
  loading.value = true
  saved.value = false
  error.value = ''
  try { const value = await openCodeUsageAPI.saveSettings({ ...settings.value }); if (version === generation) { settings.value = value; saved.value = true } }
  catch (err) { if (version === generation) error.value = extractApiErrorMessage(err, t('openCodeUsage.failed')) }
  finally { if (version === generation) loading.value = false }
}
onMounted(load)
onBeforeUnmount(() => { generation++ })
</script>
