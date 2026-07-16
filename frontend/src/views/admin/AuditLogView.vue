<template>
  <AppLayout>
    <TablePageLayout>
      <template #actions>
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div class="flex flex-1 flex-wrap items-center gap-3">
            <div class="relative min-w-64 flex-1 max-w-md">
              <Icon name="search" size="sm" class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-400" />
              <input v-model.trim="filters.q" class="input pl-9" :placeholder="t('admin.audit.searchPlaceholder')" @keyup.enter="applyFilters" />
            </div>
            <select v-model="filters.success" class="input w-36" @change="applyFilters">
              <option value="">{{ t('admin.audit.allResults') }}</option>
              <option value="true">{{ t('admin.audit.success') }}</option>
              <option value="false">{{ t('admin.audit.failed') }}</option>
            </select>
            <input v-model.trim="filters.action" class="input w-56" :placeholder="t('admin.audit.actionPlaceholder')" @keyup.enter="applyFilters" />
          </div>
          <div class="flex gap-2">
            <button class="btn btn-secondary" type="button" :title="t('common.refresh')" @click="load"><Icon name="refresh" size="sm" /></button>
            <button class="btn btn-danger" type="button" @click="showClear = true"><Icon name="trash" size="sm" />{{ t('admin.audit.clearAll') }}</button>
          </div>
        </div>
      </template>

      <template #table>
        <div class="table-wrapper">
          <table>
            <thead><tr>
              <th>{{ t('admin.audit.time') }}</th><th>{{ t('admin.audit.actor') }}</th><th>{{ t('admin.audit.action') }}</th>
              <th>{{ t('admin.audit.request') }}</th><th>{{ t('admin.audit.client') }}</th><th>{{ t('admin.audit.result') }}</th><th></th>
            </tr></thead>
            <tbody>
              <tr v-if="loading"><td colspan="7" class="py-10 text-center text-gray-500">{{ t('common.loading') }}</td></tr>
              <tr v-else-if="logs.length === 0"><td colspan="7" class="py-10 text-center text-gray-500">{{ t('admin.audit.empty') }}</td></tr>
              <template v-else>
              <tr v-for="row in logs" :key="row.id">
                <td class="whitespace-nowrap">{{ formatTime(row.created_at) }}</td>
                <td><div class="font-medium text-gray-900 dark:text-white">{{ row.actor_email || `#${row.actor_user_id || '-'}` }}</div><div class="text-xs text-gray-500">{{ row.auth_method }}</div></td>
                <td><code class="text-xs">{{ row.action }}</code></td>
                <td><div class="font-medium">{{ row.method }}</div><div class="max-w-72 truncate text-xs text-gray-500" :title="row.path">{{ row.path }}</div></td>
                <td><div>{{ row.client_ip || '-' }}</div><div class="max-w-56 truncate text-xs text-gray-500" :title="row.user_agent">{{ row.user_agent }}</div></td>
                <td><span class="inline-flex rounded px-2 py-0.5 text-xs font-medium" :class="row.status_code < 400 ? 'bg-green-50 text-green-700 dark:bg-green-900/30 dark:text-green-300' : 'bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-300'">{{ row.status_code }}</span><div class="mt-1 text-xs text-gray-500">{{ row.latency_ms }} ms</div></td>
                <td><button class="btn btn-ghost btn-sm" type="button" :title="t('admin.audit.details')" @click="openDetail(row.id)"><Icon name="eye" size="sm" /></button></td>
              </tr>
              </template>
            </tbody>
          </table>
        </div>
      </template>

      <template #pagination><Pagination v-if="pagination.total > 0" :page="pagination.page" :total="pagination.total" :page-size="pagination.page_size" @update:page="changePage" @update:pageSize="changePageSize" /></template>
    </TablePageLayout>

    <BaseDialog :show="!!detail" :title="t('admin.audit.details')" width="wide" @close="detail = null">
      <dl v-if="detail" class="grid grid-cols-1 gap-4 text-sm md:grid-cols-2">
        <div><dt class="text-gray-500">{{ t('admin.audit.action') }}</dt><dd class="font-mono">{{ detail.action }}</dd></div>
        <div><dt class="text-gray-500">{{ t('admin.audit.requestId') }}</dt><dd class="font-mono break-all">{{ detail.request_id || '-' }}</dd></div>
        <div class="md:col-span-2"><dt class="text-gray-500">{{ t('admin.audit.credential') }}</dt><dd class="font-mono">{{ detail.credential_masked || '-' }}</dd></div>
        <div class="md:col-span-2"><dt class="text-gray-500">{{ t('admin.audit.body') }}</dt><dd><pre class="mt-1 max-h-80 overflow-auto rounded bg-gray-50 p-3 text-xs dark:bg-dark-900">{{ prettyBody(detail.request_body) }}</pre></dd></div>
        <div class="md:col-span-2"><dt class="text-gray-500">{{ t('admin.audit.extra') }}</dt><dd><pre class="mt-1 overflow-auto rounded bg-gray-50 p-3 text-xs dark:bg-dark-900">{{ JSON.stringify(detail.extra || {}, null, 2) }}</pre></dd></div>
      </dl>
    </BaseDialog>

    <BaseDialog :show="showClear" :title="t('admin.audit.clearAll')" width="narrow" @close="closeClear">
      <p class="text-sm text-gray-500">{{ t('admin.audit.clearHint') }}</p>
      <input v-model.trim="totpCode" class="input mt-4 text-center text-lg" inputmode="numeric" maxlength="6" autocomplete="one-time-code" :placeholder="t('admin.audit.totpPlaceholder')" @keyup.enter="clearAll" />
      <template #footer><button class="btn btn-secondary" type="button" @click="closeClear">{{ t('common.cancel') }}</button><button class="btn btn-danger" type="button" :disabled="clearing || totpCode.length !== 6" @click="clearAll">{{ t('admin.audit.clearAll') }}</button></template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI, type AuditLog } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { getPersistedPageSize } from '@/composables/usePersistedPageSize'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()
const appStore = useAppStore()
const logs = ref<AuditLog[]>([])
const detail = ref<AuditLog | null>(null)
const loading = ref(false)
const showClear = ref(false)
const clearing = ref(false)
const totpCode = ref('')
const filters = reactive({ q: '', action: '', success: '' })
const pagination = reactive({ page: 1, page_size: getPersistedPageSize(), total: 0 })

async function load() {
  loading.value = true
  try {
    const result = await adminAPI.audit.list({ page: pagination.page, page_size: pagination.page_size, q: filters.q || undefined, action: filters.action || undefined, success: filters.success === '' ? undefined : filters.success === 'true' })
    logs.value = result.items || []
    pagination.total = result.total
  } catch (error) { appStore.showError(extractApiErrorMessage(error, t('admin.audit.loadFailed'))) }
  finally { loading.value = false }
}
function applyFilters() { pagination.page = 1; load() }
function changePage(page: number) { pagination.page = page; load() }
function changePageSize(size: number) { pagination.page_size = size; pagination.page = 1; load() }
async function openDetail(id: number) { try { detail.value = await adminAPI.audit.get(id) } catch (error) { appStore.showError(extractApiErrorMessage(error, t('admin.audit.loadFailed'))) } }
function closeClear() { showClear.value = false; totpCode.value = '' }
async function clearAll() { if (totpCode.value.length !== 6) return; clearing.value = true; try { const result = await adminAPI.audit.clear(totpCode.value); appStore.showSuccess(t('admin.audit.cleared', { count: result.deleted })); closeClear(); pagination.page = 1; await load() } catch (error) { appStore.showError(extractApiErrorMessage(error, t('admin.audit.clearFailed'))) } finally { clearing.value = false } }
function formatTime(value: string) { return new Date(value).toLocaleString() }
function prettyBody(value?: string) { if (!value) return '-'; try { return JSON.stringify(JSON.parse(value), null, 2) } catch { return value } }
onMounted(load)
</script>
