<template>
  <BaseDialog :show="show" :title="t('usage.errors.detail.title')" width="wide" @close="emit('update:show', false)">
    <div v-if="loading" class="flex justify-center py-10 text-gray-500">
      {{ t('common.loading') }}
    </div>
    <div v-else-if="loadError" class="py-8 text-center text-sm text-red-500">
      {{ t('usage.errors.detail.loadFailed') }}
    </div>
    <div v-else-if="detail" class="space-y-4 text-sm">
      <div class="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2">
        <div>
          <span class="font-medium text-gray-500 dark:text-dark-400">{{ t('usage.errors.time') }}</span>
          <p class="mt-0.5 text-gray-900 dark:text-dark-100">{{ formatDateTime(detail.created_at) }}</p>
        </div>
        <div>
          <span class="font-medium text-gray-500 dark:text-dark-400">{{ t('admin.usage.user') }}</span>
          <p class="mt-0.5 text-gray-900 dark:text-dark-100">{{ detail.user_email || '-' }} <span v-if="detail.user_id" class="text-gray-400">#{{ detail.user_id }}</span></p>
        </div>
        <div>
          <span class="font-medium text-gray-500 dark:text-dark-400">{{ t('usage.model') }}</span>
          <p class="mt-0.5 text-gray-900 dark:text-dark-100">{{ detail.model || '-' }}</p>
        </div>
        <div>
          <span class="font-medium text-gray-500 dark:text-dark-400">{{ t('usage.errors.status') }}</span>
          <p class="mt-0.5">
            <span class="badge" :class="statusClass(detail.status_code)">{{ detail.status_code || '-' }}</span>
          </p>
        </div>
        <div>
          <span class="font-medium text-gray-500 dark:text-dark-400">{{ t('usage.inboundEndpoint') }}</span>
          <p class="mt-0.5 break-all text-gray-900 dark:text-dark-100">{{ detail.inbound_endpoint || detail.request_path || '-' }}</p>
        </div>
        <div>
          <span class="font-medium text-gray-500 dark:text-dark-400">{{ t('usage.upstreamEndpoint') }}</span>
          <p class="mt-0.5 break-all text-gray-900 dark:text-dark-100">{{ detail.upstream_endpoint || '-' }}</p>
        </div>
        <div v-if="detail.upstream_status_code != null">
          <span class="font-medium text-gray-500 dark:text-dark-400">{{ t('usage.errors.detail.upstreamStatus') }}</span>
          <p class="mt-0.5 text-gray-900 dark:text-dark-100">{{ detail.upstream_status_code }}</p>
        </div>
        <div>
          <span class="font-medium text-gray-500 dark:text-dark-400">{{ t('usage.errors.platform') }}</span>
          <p class="mt-0.5 text-gray-900 dark:text-dark-100">{{ detail.platform || '-' }}</p>
        </div>
      </div>

      <div v-if="detail.message">
        <span class="font-medium text-gray-500 dark:text-dark-400">{{ t('usage.errors.message') }}</span>
        <p class="mt-0.5 break-all text-gray-900 dark:text-dark-100">{{ detail.message }}</p>
      </div>

      <div v-if="detail.error_body">
        <span class="font-medium text-gray-500 dark:text-dark-400">{{ t('usage.errors.detail.responseBody') }}</span>
        <pre class="mt-1 max-h-[42vh] overflow-auto whitespace-pre-wrap break-all rounded border border-gray-200 bg-gray-50 p-3 text-xs text-gray-800 dark:border-dark-700 dark:bg-dark-900 dark:text-dark-200">{{ detail.error_body }}</pre>
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { opsAPI, type OpsErrorDetail } from '@/api/admin/ops'
import { formatDateTime } from '@/utils/format'

const props = defineProps<{
  show: boolean
  errorId: number | null
}>()

const emit = defineEmits<{
  (e: 'update:show', v: boolean): void
}>()

const { t } = useI18n()
const loading = ref(false)
const loadError = ref(false)
const detail = ref<OpsErrorDetail | null>(null)

watch(
  () => [props.show, props.errorId] as const,
  ([show, id]) => {
    if (show && id != null) {
      fetchDetail(id)
    } else if (!show) {
      detail.value = null
      loadError.value = false
    }
  }
)

async function fetchDetail(id: number) {
  loading.value = true
  loadError.value = false
  detail.value = null
  try {
    detail.value = await opsAPI.getErrorLogDetail(id)
  } catch (e) {
    console.error('[UsageErrorDetailModal] Failed to load error detail:', e)
    loadError.value = true
  } finally {
    loading.value = false
  }
}

function statusClass(code: number) {
  if (code >= 500) return 'badge-danger'
  if (code === 429) return 'badge-warning'
  return 'badge-gray'
}
</script>
