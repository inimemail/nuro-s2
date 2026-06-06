<template>
  <div class="card overflow-hidden">
    <div class="overflow-auto">
      <table class="min-w-full text-sm">
        <thead class="bg-gray-50 dark:bg-dark-800">
          <tr>
            <th class="px-4 py-3 text-left">{{ t('usage.errors.time') }}</th>
            <th class="px-4 py-3 text-left">{{ t('admin.usage.user') }}</th>
            <th class="px-4 py-3 text-left">{{ t('usage.apiKeyFilter') }}</th>
            <th class="px-4 py-3 text-left">{{ t('usage.model') }}</th>
            <th class="px-4 py-3 text-left">{{ t('usage.endpoint') }}</th>
            <th class="px-4 py-3 text-left">{{ t('usage.errors.status') }}</th>
            <th class="px-4 py-3 text-left">{{ t('usage.errors.category') }}</th>
            <th class="px-4 py-3 text-left">{{ t('usage.errors.message') }}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 bg-white dark:divide-dark-700 dark:bg-dark-900">
          <tr v-if="loading">
            <td colspan="8" class="px-4 py-10 text-center text-gray-500 dark:text-gray-400">
              {{ t('common.loading') }}
            </td>
          </tr>
          <template v-else>
            <tr
              v-for="row in data"
              :key="row.id"
              class="cursor-pointer hover:bg-gray-50 dark:hover:bg-dark-800"
              @click="openDetail(row.id)"
            >
              <td class="whitespace-nowrap px-4 py-3 text-gray-600 dark:text-gray-400">
                {{ formatDateTime(row.created_at) }}
              </td>
              <td class="px-4 py-3">
                <div class="font-medium text-gray-900 dark:text-white">{{ row.user_email || '-' }}</div>
                <div v-if="row.user_id" class="text-xs text-gray-400">#{{ row.user_id }}</div>
              </td>
              <td class="px-4 py-3">
                <div>{{ row.api_key_name || (row.api_key_id ? `#${row.api_key_id}` : '-') }}</div>
                <span
                  v-if="row.api_key_deleted"
                  class="inline-flex items-center rounded bg-gray-100 px-1 py-px text-[10px] font-medium text-gray-500 dark:bg-dark-700 dark:text-gray-400"
                >
                  {{ t('usage.errors.keyDeleted') }}
                </span>
              </td>
              <td class="max-w-[220px] break-all px-4 py-3 font-medium text-gray-900 dark:text-white">
                {{ row.model || '-' }}
              </td>
              <td class="max-w-[260px] break-all px-4 py-3 text-xs text-gray-600 dark:text-gray-300">
                {{ row.inbound_endpoint || row.request_path || '-' }}
              </td>
              <td class="px-4 py-3">
                <span class="badge" :class="statusClass(row.status_code)">{{ row.status_code || '-' }}</span>
              </td>
              <td class="px-4 py-3">{{ categoryLabel(row) }}</td>
              <td class="max-w-[360px] truncate px-4 py-3" :title="row.message">
                {{ row.message || '-' }}
              </td>
            </tr>
          </template>
          <tr v-if="!loading && data.length === 0">
            <td colspan="8" class="px-4 py-10 text-center text-gray-500 dark:text-gray-400">
              {{ t('usage.errors.empty') }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <Pagination
      v-if="total > 0"
      :page="page"
      :page-size="pageSize"
      :total="total"
      @update:page="$emit('update:page', $event)"
      @update:pageSize="$emit('update:pageSize', $event)"
    />

    <UsageErrorDetailModal v-model:show="showDetail" :error-id="selectedId" />
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Pagination from '@/components/common/Pagination.vue'
import UsageErrorDetailModal from '@/components/admin/usage/UsageErrorDetailModal.vue'
import { formatDateTime } from '@/utils/format'
import type { OpsErrorLog } from '@/api/admin/ops'

defineProps<{
  data: OpsErrorLog[]
  loading: boolean
  total: number
  page: number
  pageSize: number
}>()

defineEmits<{
  (e: 'update:page', v: number): void
  (e: 'update:pageSize', v: number): void
}>()

const { t } = useI18n()
const showDetail = ref(false)
const selectedId = ref<number | null>(null)

function openDetail(id: number) {
  selectedId.value = id
  showDetail.value = true
}

function statusClass(code: number) {
  if (code >= 500) return 'badge-danger'
  if (code === 429) return 'badge-warning'
  return 'badge-gray'
}

function categoryLabel(row: OpsErrorLog) {
  if (row.error_owner === 'client') return t('usage.errors.categories.invalid_request')
  if (row.type === 'rate_limit' || row.status_code === 429) return t('usage.errors.categories.rate_limit')
  if (row.error_owner === 'provider') return t('usage.errors.categories.upstream')
  if (row.error_owner === 'platform') return t('usage.errors.categories.internal')
  return row.type || t('usage.errors.categories.other')
}
</script>
