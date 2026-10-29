<template>
  <section class="rounded-2xl border border-gray-200 bg-gray-50/70 p-4 dark:border-dark-600 dark:bg-dark-800/50">
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div class="min-w-0">
        <h4 class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('admin.accounts.grokCatalog.title') }}</h4>
        <p class="mt-1 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('admin.accounts.grokCatalog.hint') }}</p>
      </div>
      <div class="flex flex-wrap gap-2">
        <button type="button" class="btn btn-secondary shrink-0 text-xs" :disabled="loading" @click="load">{{ t(loading ? 'common.loading' : 'admin.accounts.grokCatalog.inspect') }}</button>
        <button v-if="loading" type="button" class="btn btn-secondary text-xs" @click="invalidate">{{ t('common.cancel') }}</button>
      </div>
    </div>
    <p v-if="error" role="alert" class="mt-3 break-words text-xs text-red-600 dark:text-red-400">{{ error }}</p>
    <div v-if="data" class="mt-4 space-y-3" aria-live="polite">
      <div class="flex flex-wrap gap-x-5 gap-y-2 rounded-xl bg-white p-3 text-xs dark:bg-dark-900">
        <span class="font-medium text-gray-800 dark:text-gray-200">{{ t(`admin.accounts.grokCatalog.${data.status}`) }}</span>
        <span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.grokCatalog.count', { count: data.catalog_count }) }}</span>
        <time v-if="data.succeeded_at" :datetime="data.succeeded_at" class="break-all text-gray-500 dark:text-gray-400">{{ new Date(data.succeeded_at).toLocaleString() }}</time>
      </div>
      <details v-if="data.models.length" class="rounded-xl border border-gray-200 p-3 dark:border-dark-600">
        <summary class="cursor-pointer text-xs font-medium text-gray-700 focus-visible:outline-blue-500 dark:text-gray-300">{{ t('admin.accounts.grokCatalog.details', { count: data.models.length }) }}</summary>
        <ul class="mt-3 divide-y divide-gray-100 dark:divide-dark-600">
          <li v-for="model in data.models" :key="model.model" class="flex flex-wrap items-center justify-between gap-2 py-2 text-xs">
            <code class="min-w-0 break-all text-gray-700 dark:text-gray-300">{{ model.model }}</code>
            <div class="flex flex-wrap items-center gap-2">
              <span class="rounded-md bg-gray-100 px-2 py-1 text-gray-600 dark:bg-dark-700 dark:text-gray-300">{{ t(`admin.accounts.grokCatalog.${model.status}`) }}</span>
              <button type="button" class="btn btn-secondary text-xs" :aria-label="t('admin.accounts.grokCatalog.copyModel', { model: model.model })" @click="copyToClipboard(model.model)">{{ t('common.copy') }}</button>
            </div>
          </li>
        </ul>
      </details>
    </div>
  </section>
</template>
<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getGrokModelDiagnostics, type GrokModelDiagnosticsResult } from '@/api/admin/accounts'
import { extractSafeApiErrorMessage, isApiRequestCancelled } from '@/utils/apiError'
import { useClipboard } from '@/composables/useClipboard'
const props = defineProps<{ accountId: number; active: boolean }>()
const { t } = useI18n()
const { copyToClipboard } = useClipboard()
const data = ref<GrokModelDiagnosticsResult>()
const error = ref('')
const loading = ref(false)
let generation = 0
let controller: AbortController | undefined
function invalidate() { generation++; controller?.abort(); controller = undefined; loading.value = false; data.value = undefined; error.value = '' }
watch(() => [props.accountId, props.active], invalidate, { flush: 'sync' })
onBeforeUnmount(invalidate)
async function load() {
  if (!props.active || loading.value) return
  const current = ++generation
  const request = new AbortController()
  controller = request
  loading.value = true
  error.value = ''
  try {
    const result = await getGrokModelDiagnostics(props.accountId, request.signal)
    if (current === generation && !request.signal.aborted) data.value = result
  } catch (err) {
    if (current === generation && !request.signal.aborted && !isApiRequestCancelled(err)) error.value = extractSafeApiErrorMessage(err, t('admin.accounts.syncUpstreamModelsFailed'))
  } finally {
    if (current === generation) { loading.value = false; controller = undefined }
  }
}
</script>
