<template>
  <AppLayout>
    <div class="space-y-6">
      <header class="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
        <div class="min-w-0">
          <div class="flex flex-wrap items-center gap-2">
            <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">{{ copy.title }}</h1>
            <span
              class="inline-flex rounded px-2 py-1 text-xs font-medium"
              :class="form?.enabled ? 'bg-green-50 text-green-700 dark:bg-green-900/30 dark:text-green-300' : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'"
            >
              {{ form?.enabled ? copy.enabled : copy.disabled }}
            </span>
            <span class="inline-flex rounded bg-sky-50 px-2 py-1 text-xs font-medium text-sky-700 dark:bg-sky-900/30 dark:text-sky-300">
              {{ form?.enabled ? copy.asyncMode : copy.offMode }}
            </span>
          </div>
          <p class="mt-1 max-w-3xl text-sm text-gray-500 dark:text-gray-400">{{ copy.description }}</p>
          <p v-if="configUpdatedAt" class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ copy.lastUpdated }}: {{ formatTime(configUpdatedAt) }}</p>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <button type="button" class="btn btn-secondary inline-flex items-center gap-2" :disabled="initialLoading" :title="copy.refresh" @click="refreshActiveTab">
            <Icon name="refresh" size="sm" :class="initialLoading || eventsLoading ? 'animate-spin' : ''" />
            {{ copy.refresh }}
          </button>
          <button v-if="activeTab === 'configuration'" type="button" class="btn btn-primary inline-flex items-center gap-2" :disabled="saving || !form" @click="saveConfiguration">
            <Icon name="check" size="sm" />
            {{ saving ? copy.saving : copy.save }}
          </button>
        </div>
      </header>

      <div class="rounded-md border border-sky-200 bg-sky-50 px-4 py-3 text-sm text-sky-800 dark:border-sky-900/60 dark:bg-sky-900/20 dark:text-sky-200">
        <div class="flex items-start gap-2">
          <Icon name="clock" size="sm" class="mt-0.5 flex-shrink-0" />
          <p>{{ copy.asyncExplanation }}</p>
        </div>
      </div>

      <section aria-labelledby="prompt-audit-runtime-title">
        <div class="mb-3 flex items-center justify-between">
          <h2 id="prompt-audit-runtime-title" class="text-sm font-semibold text-gray-700 dark:text-gray-200">{{ copy.runtime }}</h2>
          <span v-if="safeRuntimeError" class="font-mono text-xs text-red-600 dark:text-red-400">{{ safeRuntimeError }}</span>
        </div>
        <div class="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
          <div v-for="metric in runtimeMetrics" :key="metric.label" class="rounded-md border border-gray-100 bg-white px-4 py-3 shadow-sm dark:border-dark-700 dark:bg-dark-800">
            <p class="truncate text-xs font-medium text-gray-500 dark:text-gray-400">{{ metric.label }}</p>
            <p class="mt-1 truncate text-xl font-semibold text-gray-900 dark:text-white">{{ metric.value }}</p>
            <p v-if="metric.meta" class="mt-1 truncate text-xs text-gray-400 dark:text-gray-500">{{ metric.meta }}</p>
          </div>
        </div>
      </section>

      <div class="flex border-b border-gray-200 dark:border-dark-700" role="tablist">
        <button
          type="button"
          class="border-b-2 px-4 py-2.5 text-sm font-medium transition-colors"
          :class="activeTab === 'configuration' ? 'border-primary-500 text-primary-600 dark:text-primary-400' : 'border-transparent text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200'"
          role="tab"
          :aria-selected="activeTab === 'configuration'"
          @click="activeTab = 'configuration'"
        >
          {{ copy.configuration }}
        </button>
        <button
          type="button"
          class="border-b-2 px-4 py-2.5 text-sm font-medium transition-colors"
          :class="activeTab === 'events' ? 'border-primary-500 text-primary-600 dark:text-primary-400' : 'border-transparent text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200'"
          role="tab"
          :aria-selected="activeTab === 'events'"
          @click="activateEvents"
        >
          {{ copy.events }}
          <span v-if="eventList.total > 0" class="ml-1 text-xs text-gray-400">{{ eventList.total }}</span>
        </button>
      </div>

      <div v-if="initialLoading && !form" class="flex items-center justify-center py-16">
        <div class="h-8 w-8 animate-spin rounded-full border-b-2 border-primary-600"></div>
      </div>

      <template v-else-if="form && activeTab === 'configuration'">
        <section class="card" aria-labelledby="prompt-audit-general-title">
          <div class="border-b border-gray-100 px-5 py-4 dark:border-dark-700 sm:px-6">
            <h2 id="prompt-audit-general-title" class="text-lg font-semibold text-gray-900 dark:text-white">{{ copy.generalSettings }}</h2>
          </div>
          <div class="space-y-6 px-5 py-5 sm:px-6">
            <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <p class="text-sm font-medium text-gray-900 dark:text-white">{{ copy.auditSwitch }}</p>
                <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ copy.asyncExplanation }}</p>
              </div>
              <Toggle v-model="form.enabled" />
            </div>

            <div class="grid grid-cols-1 gap-4 md:grid-cols-3">
              <label class="block">
                <span class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ copy.workerCount }}</span>
                <input v-model.number="form.worker_count" type="number" min="1" max="16" class="input" />
                <span class="mt-1 block text-xs text-gray-400">{{ copy.workerHint }}</span>
              </label>
              <label class="block">
                <span class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ copy.queueCapacity }}</span>
                <input v-model.number="form.queue_capacity" type="number" min="1" max="100000" class="input" />
                <span class="mt-1 block text-xs text-gray-400">{{ copy.queueHint }}</span>
              </label>
              <label class="block">
                <span class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ copy.retentionDays }}</span>
                <input v-model.number="form.retention_days" type="number" min="1" max="90" class="input" />
                <span class="mt-1 block text-xs text-gray-400">{{ copy.retentionHint }}</span>
              </label>
            </div>

            <div class="flex flex-col gap-3 border-t border-gray-100 pt-5 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <p class="text-sm font-medium text-gray-900 dark:text-white">{{ copy.storePass }}</p>
                <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ copy.storePassHint }}</p>
              </div>
              <Toggle v-model="form.store_pass_events" />
            </div>
          </div>
        </section>

        <div class="grid grid-cols-1 gap-6 xl:grid-cols-2">
          <section class="card" aria-labelledby="prompt-audit-groups-title">
            <div class="border-b border-gray-100 px-5 py-4 dark:border-dark-700 sm:px-6">
              <h2 id="prompt-audit-groups-title" class="text-lg font-semibold text-gray-900 dark:text-white">{{ copy.groupScope }}</h2>
            </div>
            <div class="px-5 py-5 sm:px-6">
              <div class="inline-flex rounded-md bg-gray-100 p-1 dark:bg-dark-700">
                <button type="button" class="rounded px-3 py-1.5 text-sm font-medium" :class="form.all_groups ? 'bg-white text-gray-900 shadow-sm dark:bg-dark-800 dark:text-white' : 'text-gray-500 dark:text-gray-300'" @click="form.all_groups = true">{{ copy.allGroups }}</button>
                <button type="button" class="rounded px-3 py-1.5 text-sm font-medium" :class="!form.all_groups ? 'bg-white text-gray-900 shadow-sm dark:bg-dark-800 dark:text-white' : 'text-gray-500 dark:text-gray-300'" @click="form.all_groups = false">{{ copy.selectedGroups }}</button>
              </div>
              <div v-if="!form.all_groups" class="mt-4 max-h-64 space-y-2 overflow-y-auto pr-1">
                <label v-for="group in groups" :key="group.id" class="flex items-center gap-3 rounded-md px-2 py-2 hover:bg-gray-50 dark:hover:bg-dark-700/60">
                  <input v-model="form.group_ids" :value="group.id" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                  <span class="min-w-0 flex-1 truncate text-sm text-gray-800 dark:text-gray-200">{{ group.name }}</span>
                  <span class="text-xs uppercase text-gray-400">{{ group.platform }}</span>
                </label>
                <p v-if="groups.length === 0" class="py-4 text-center text-sm text-gray-500">{{ copy.noGroups }}</p>
              </div>
            </div>
          </section>

          <section class="card" aria-labelledby="prompt-audit-scanners-title">
            <div class="border-b border-gray-100 px-5 py-4 dark:border-dark-700 sm:px-6">
              <h2 id="prompt-audit-scanners-title" class="text-lg font-semibold text-gray-900 dark:text-white">{{ copy.scanners }}</h2>
            </div>
            <div class="grid grid-cols-1 gap-2 px-5 py-5 sm:grid-cols-2 sm:px-6">
              <label v-for="scanner in scannerOptions" :key="scanner" class="flex items-center gap-3 rounded-md px-2 py-2 hover:bg-gray-50 dark:hover:bg-dark-700/60">
                <input v-model="form.scanners" :value="scanner" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-800 dark:text-gray-200">{{ copy.scannerLabels[scanner] }}</span>
              </label>
            </div>
          </section>
        </div>

        <section class="card" aria-labelledby="prompt-audit-endpoints-title">
          <div class="flex flex-col gap-3 border-b border-gray-100 px-5 py-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between sm:px-6">
            <div>
              <h2 id="prompt-audit-endpoints-title" class="text-lg font-semibold text-gray-900 dark:text-white">{{ copy.guardPool }}</h2>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ copy.guardPoolHint }}</p>
            </div>
            <button type="button" class="btn btn-secondary inline-flex items-center gap-2" @click="addEndpoint">
              <Icon name="plus" size="sm" />
              {{ copy.addEndpoint }}
            </button>
          </div>
          <p v-if="form.endpoints.length === 0" class="px-6 py-10 text-center text-sm text-gray-500 dark:text-gray-400">{{ copy.emptyEndpoints }}</p>
          <EndpointEditor
            v-for="(_, index) in form.endpoints"
            :key="endpointKeys[index]"
            v-model="form.endpoints[index]"
            :index="index"
            :copy="copy"
            :probe-loading="probingKeys.has(endpointKeys[index])"
            :probe-result="probeResults[endpointKeys[index]]"
            @probe="probe(index)"
            @remove="removeEndpoint(index)"
          />
        </section>
      </template>

      <template v-else-if="activeTab === 'events'">
        <section class="card" aria-labelledby="prompt-audit-events-title">
          <div class="space-y-4 border-b border-gray-100 px-5 py-4 dark:border-dark-700 sm:px-6">
            <div class="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
              <h2 id="prompt-audit-events-title" class="text-lg font-semibold text-gray-900 dark:text-white">{{ copy.filters }}</h2>
              <div class="flex flex-wrap items-center gap-2">
                <span v-if="selectedEventIDs.length" class="text-sm text-gray-500 dark:text-gray-400">{{ interpolate(copy.selectedCount, { count: selectedEventIDs.length }) }}</span>
                <button v-if="selectedEventIDs.length" type="button" class="btn btn-danger btn-sm inline-flex items-center gap-1.5" @click="openBatchDelete">
                  <Icon name="trash" size="sm" />{{ copy.deleteSelected }}
                </button>
                <button type="button" class="btn btn-secondary btn-sm inline-flex items-center gap-1.5 text-red-600 dark:text-red-400" :disabled="previewingDelete" @click="prepareFilteredDelete">
                  <Icon name="filter" size="sm" />{{ copy.deleteFiltered }}
                </button>
              </div>
            </div>

            <div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-6">
              <label class="block">
                <span class="mb-1 block text-xs font-medium text-gray-500">{{ copy.decision }}</span>
                <select v-model="filterForm.decision" class="input" @change="applyFilters">
                  <option value="">{{ copy.allDecisions }}</option>
                  <option value="pass">{{ copy.pass }}</option>
                  <option value="flag">{{ copy.flag }}</option>
                  <option value="critical">{{ copy.critical }}</option>
                  <option value="unavailable">{{ copy.unavailable }}</option>
                </select>
              </label>
              <label class="block">
                <span class="mb-1 block text-xs font-medium text-gray-500">{{ copy.risk }}</span>
                <select v-model="filterForm.risk_level" class="input" @change="applyFilters">
                  <option value="">{{ copy.allRisks }}</option>
                  <option value="low">{{ copy.low }}</option>
                  <option value="medium">{{ copy.medium }}</option>
                  <option value="high">{{ copy.high }}</option>
                  <option value="critical">{{ copy.critical }}</option>
                  <option value="unknown">{{ copy.unknown }}</option>
                </select>
              </label>
              <label class="block">
                <span class="mb-1 block text-xs font-medium text-gray-500">{{ copy.group }}</span>
                <select v-model.number="filterForm.group_id" class="input" @change="applyFilters">
                  <option :value="0">{{ copy.allGroupsFilter }}</option>
                  <option v-for="group in groups" :key="group.id" :value="group.id">{{ group.name }}</option>
                </select>
              </label>
              <label class="block">
                <span class="mb-1 block text-xs font-medium text-gray-500">{{ copy.userId }}</span>
                <input v-model.trim="filterForm.user_id" class="input" inputmode="numeric" @keyup.enter="applyFilters" />
              </label>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-xs font-medium text-gray-500">{{ copy.search }}</span>
                <div class="relative">
                  <Icon name="search" size="sm" class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-400" />
                  <input v-model.trim="filterForm.search" class="input pl-9" :placeholder="copy.searchPlaceholder" @keyup.enter="applyFilters" />
                </div>
              </label>
            </div>
            <div class="flex justify-end gap-2">
              <button type="button" class="btn btn-ghost btn-sm" @click="resetFilters">{{ copy.reset }}</button>
              <button type="button" class="btn btn-primary btn-sm" @click="applyFilters">{{ copy.apply }}</button>
            </div>
          </div>

          <div class="overflow-x-auto">
            <table class="min-w-[1100px] w-full divide-y divide-gray-200 dark:divide-dark-700">
              <thead class="bg-gray-50 dark:bg-dark-800">
                <tr>
                  <th class="w-10 px-4 py-3 text-left">
                    <input type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" :checked="allPageSelected" :aria-label="copy.selectedCount" @change="toggleCurrentPage" />
                  </th>
                  <th class="px-4 py-3 text-left text-xs font-medium uppercase text-gray-500">{{ copy.time }}</th>
                  <th class="px-4 py-3 text-left text-xs font-medium uppercase text-gray-500">{{ copy.decision }}</th>
                  <th class="px-4 py-3 text-left text-xs font-medium uppercase text-gray-500">{{ copy.identity }}</th>
                  <th class="px-4 py-3 text-left text-xs font-medium uppercase text-gray-500">{{ copy.group }}</th>
                  <th class="px-4 py-3 text-left text-xs font-medium uppercase text-gray-500">{{ copy.route }}</th>
                  <th class="px-4 py-3 text-left text-xs font-medium uppercase text-gray-500">{{ copy.preview }}</th>
                  <th class="px-4 py-3 text-left text-xs font-medium uppercase text-gray-500">{{ copy.scanner }}</th>
                  <th class="px-4 py-3 text-right text-xs font-medium uppercase text-gray-500">{{ copy.actions }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 bg-white dark:divide-dark-700 dark:bg-dark-800">
                <tr v-if="eventsLoading"><td colspan="9" class="px-4 py-12 text-center text-sm text-gray-500">{{ copy.loading }}</td></tr>
                <tr v-else-if="eventList.items.length === 0"><td colspan="9" class="px-4 py-12 text-center text-sm text-gray-500">{{ copy.noEvents }}</td></tr>
                <tr v-for="event in eventList.items" v-else :key="event.id" class="hover:bg-gray-50 dark:hover:bg-dark-700/60">
                  <td class="px-4 py-3"><input v-model="selectedEventIDs" :value="event.id" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" /></td>
                  <td class="whitespace-nowrap px-4 py-3 text-sm text-gray-700 dark:text-gray-300">{{ formatTime(event.created_at) }}</td>
                  <td class="whitespace-nowrap px-4 py-3">
                    <span class="inline-flex rounded px-2 py-0.5 text-xs font-medium" :class="decisionClass(event.decision)">{{ decisionLabel(event.decision) }}</span>
                    <div class="mt-1 text-xs text-gray-400">{{ riskLabel(event.risk_level) }}</div>
                  </td>
                  <td class="max-w-52 px-4 py-3 text-sm text-gray-700 dark:text-gray-300">
                    <div class="truncate">{{ event.user_email || `UID ${event.user_id || '-'}` }}</div>
                    <div class="truncate text-xs text-gray-400">{{ event.api_key_name || `Key #${event.api_key_id || '-'}` }}</div>
                  </td>
                  <td class="max-w-40 px-4 py-3 text-sm text-gray-700 dark:text-gray-300"><div class="truncate">{{ event.group_name || '-' }}</div></td>
                  <td class="max-w-56 px-4 py-3 text-sm text-gray-700 dark:text-gray-300">
                    <div class="truncate font-mono text-xs">{{ event.endpoint || '-' }}</div>
                    <div class="truncate text-xs text-gray-400">{{ event.provider || '-' }} / {{ event.model || '-' }}</div>
                  </td>
                  <td class="max-w-72 px-4 py-3 text-sm text-gray-700 dark:text-gray-300"><p class="line-clamp-2 whitespace-pre-wrap break-words">{{ event.redacted_preview || '-' }}</p></td>
                  <td class="whitespace-nowrap px-4 py-3 text-sm text-gray-700 dark:text-gray-300">
                    <div>{{ event.guard_endpoint_id || '-' }}</div>
                    <div class="text-xs text-gray-400">{{ event.latency_ms }} ms</div>
                  </td>
                  <td class="whitespace-nowrap px-4 py-3 text-right">
                    <button type="button" class="btn btn-ghost btn-sm" :title="copy.details" @click="openDetail(event.id)"><Icon name="eye" size="sm" /></button>
                    <button type="button" class="btn btn-ghost btn-sm text-red-600 dark:text-red-400" :title="copy.delete" @click="openSingleDelete(event.id)"><Icon name="trash" size="sm" /></button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <Pagination
            v-if="eventList.total > 0"
            :page="eventList.page"
            :page-size="eventList.page_size"
            :total="eventList.total"
            @update:page="changePage"
            @update:page-size="changePageSize"
          />
        </section>
      </template>
    </div>

    <EventDetailDialog :event="detailEvent" :copy="copy" @close="detailEvent = null" />

    <BaseDialog :show="Boolean(pendingDelete)" :title="copy.confirmDelete" width="narrow" @close="closeDeleteDialog">
      <p class="text-sm text-gray-600 dark:text-gray-300">
        {{ pendingDelete?.kind === 'single' ? copy.deleteOneHint : interpolate(copy.deleteBatchHint, { count: pendingDelete?.ids.length || 0 }) }}
      </p>
      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="deleting" @click="closeDeleteDialog">{{ copy.cancel }}</button>
        <button type="button" class="btn btn-danger" :disabled="deleting" @click="confirmExplicitDelete">{{ copy.confirmDelete }}</button>
      </template>
    </BaseDialog>

    <BaseDialog :show="Boolean(deletePreview)" :title="copy.deletePreviewTitle" width="narrow" @close="closeDeletePreview">
      <template v-if="deletePreview">
        <p class="text-sm text-gray-600 dark:text-gray-300">{{ interpolate(copy.deletePreviewHint, { count: deletePreview.count }) }}</p>
        <p class="mt-3 text-xs text-gray-500 dark:text-gray-400">{{ interpolate(copy.previewExpires, { time: formatTime(deletePreview.expires_at) }) }}</p>
      </template>
      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="deleting" @click="closeDeletePreview">{{ copy.cancel }}</button>
        <button type="button" class="btn btn-danger" :disabled="deleting || !deletePreview" @click="confirmFilteredDelete">{{ copy.confirmDelete }}</button>
      </template>
    </BaseDialog>

    <TotpStepUpDialog :controller="stepUp" />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { groupsAPI } from '@/api/admin'
import type { AdminGroup } from '@/types'
import { useAppStore } from '@/stores/app'
import { isStepUpCancelled, useStepUp } from '@/composables/useStepUp'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Pagination from '@/components/common/Pagination.vue'
import Toggle from '@/components/common/Toggle.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import EndpointEditor from './components/EndpointEditor.vue'
import EventDetailDialog from './components/EventDetailDialog.vue'
import promptAuditAPI from './api'
import { usePromptAuditCopy } from './copy'
import { promptAuditErrorCode, promptAuditErrorMessage } from './errors'
import { configToForm, createEndpointForm, deleteFilter, displayableUpdatedAt, endpointToUpdate, formToUpdate, scannerOptions } from './model'
import type {
  PromptAuditConfigForm,
  PromptAuditDeletePreview,
  PromptAuditEvent,
  PromptAuditEventFilter,
  PromptAuditEventList,
  PromptAuditProbeResult,
  PromptAuditRuntime
} from './types'

type Tab = 'configuration' | 'events'
type PendingDelete = { kind: 'single' | 'batch'; ids: number[] }

const { locale } = useI18n()
const copyRef = usePromptAuditCopy(locale)
const copy = computed(() => copyRef.value)
const appStore = useAppStore()
const stepUp = useStepUp()

const activeTab = ref<Tab>('configuration')
const initialLoading = ref(true)
const eventsLoading = ref(false)
const saving = ref(false)
const deleting = ref(false)
const previewingDelete = ref(false)
const form = ref<PromptAuditConfigForm | null>(null)
const configUpdatedAt = ref('')
const runtime = ref<PromptAuditRuntime | null>(null)
const groups = ref<AdminGroup[]>([])
const endpointKeys = ref<string[]>([])
const probingKeys = ref(new Set<string>())
const probeResults = ref<Record<string, PromptAuditProbeResult>>({})
const detailEvent = ref<PromptAuditEvent | null>(null)
const pendingDelete = ref<PendingDelete | null>(null)
const deletePreview = ref<PromptAuditDeletePreview | null>(null)
const selectedEventIDs = ref<number[]>([])
const eventsLoaded = ref(false)
const eventList = reactive<PromptAuditEventList>({ items: [], total: 0, page: 1, page_size: 20 })
const filterForm = reactive({ decision: '', risk_level: '', group_id: 0, user_id: '', search: '' })
let runtimeTimer: number | undefined

const runtimeMetrics = computed(() => [
  { label: copy.value.queue, value: runtime.value ? `${runtime.value.queue_length} / ${runtime.value.queue_capacity}` : '-', meta: runtime.value?.mode === 'async_audit' ? copy.value.asyncMode : copy.value.offMode },
  { label: copy.value.workers, value: runtime.value?.worker_count ?? '-', meta: '' },
  { label: copy.value.enqueued, value: runtime.value?.enqueued ?? '-', meta: '' },
  { label: copy.value.processed, value: runtime.value?.processed ?? '-', meta: '' },
  { label: copy.value.dropped, value: runtime.value?.dropped ?? '-', meta: '' },
  { label: copy.value.failed, value: runtime.value?.failed ?? '-', meta: '' }
])

const safeRuntimeError = computed(() => {
  const value = runtime.value?.last_error || ''
  return /^[a-z0-9_]{1,80}$/i.test(value) ? value : ''
})

const allPageSelected = computed(() => eventList.items.length > 0 && eventList.items.every((event) => selectedEventIDs.value.includes(event.id)))

function interpolate(template: string, values: Record<string, string | number>): string {
  return template.replace(/\{(\w+)\}/g, (_, key: string) => String(values[key] ?? ''))
}

function safeError(error: unknown, fallback: string): string {
  return promptAuditErrorMessage(error, locale.value, fallback)
}

function showActionError(error: unknown, fallback: string) {
  if (!isStepUpCancelled(error)) appStore.showError(safeError(error, fallback))
}

function makeEndpointKey(): string {
  return globalThis.crypto?.randomUUID?.() || `endpoint-${Date.now()}-${Math.random().toString(36).slice(2)}`
}

async function initialize() {
  initialLoading.value = true
  const [configResult, runtimeResult, groupsResult] = await Promise.allSettled([
    promptAuditAPI.getConfig(),
    promptAuditAPI.getRuntime(),
    groupsAPI.getAll()
  ])
  if (configResult.status === 'fulfilled') applyConfig(configResult.value)
  else appStore.showError(safeError(configResult.reason, copy.value.loadFailed))
  if (runtimeResult.status === 'fulfilled') runtime.value = runtimeResult.value
  if (groupsResult.status === 'fulfilled') groups.value = groupsResult.value || []
  initialLoading.value = false
}

function applyConfig(config: Awaited<ReturnType<typeof promptAuditAPI.getConfig>>) {
	form.value = configToForm(config)
	configUpdatedAt.value = displayableUpdatedAt(config.updated_at)
  endpointKeys.value = config.endpoints.map(() => makeEndpointKey())
  probeResults.value = {}
}

async function loadConfig() {
  try { applyConfig(await promptAuditAPI.getConfig()) }
  catch (error) { appStore.showError(safeError(error, copy.value.loadFailed)) }
}

async function loadRuntime() {
  try { runtime.value = await promptAuditAPI.getRuntime() }
  catch { /* Runtime failure is represented by the unavailable metrics. */ }
}

function validateConfiguration(value: PromptAuditConfigForm): boolean {
  if (!Number.isInteger(Number(value.worker_count)) || value.worker_count < 1 || value.worker_count > 16) return false
  if (!Number.isInteger(Number(value.queue_capacity)) || value.queue_capacity < 1 || value.queue_capacity > 100000) return false
  if (!Number.isInteger(Number(value.retention_days)) || value.retention_days < 1 || value.retention_days > 90) return false
  if (!value.all_groups && value.group_ids.length === 0) return false
  if (value.scanners.length === 0) return false
  const ids = new Set<string>()
  let enabledEndpoints = 0
  for (const endpoint of value.endpoints) {
    const id = endpoint.id.trim()
    if (!id || !endpoint.name.trim() || !endpoint.base_url.trim() || !endpoint.model.trim()) return false
    if (ids.has(id)) return false
    ids.add(id)
    if (!Number.isInteger(Number(endpoint.timeout_ms)) || endpoint.timeout_ms < 100 || endpoint.timeout_ms > 30000) return false
    if (endpoint.allow_private && !endpoint.allowed_cidrs_text.trim()) return false
    if (endpoint.enabled) enabledEndpoints++
  }
  return !value.enabled || enabledEndpoints > 0
}

async function saveConfiguration() {
  if (!form.value || saving.value) return
  if (!validateConfiguration(form.value)) {
    appStore.showError(copy.value.invalidForm)
    return
  }
  saving.value = true
  try {
    const saved = await stepUp.run(() => promptAuditAPI.updateConfig(formToUpdate(form.value!)))
    applyConfig(saved)
    appStore.showSuccess(copy.value.configSaved)
    await loadRuntime()
  } catch (error) {
    if (promptAuditErrorCode(error) === 'PROMPT_AUDIT_CONFIG_CONFLICT') await loadConfig()
    showActionError(error, copy.value.saveFailed)
  } finally {
    saving.value = false
  }
}

function addEndpoint() {
  if (!form.value) return
  const suffix = makeEndpointKey().replace(/[^a-z0-9]/gi, '').slice(-10).toLowerCase()
  form.value.endpoints.push(createEndpointForm(`guard-${suffix}`))
  endpointKeys.value.push(makeEndpointKey())
}

function removeEndpoint(index: number) {
  if (!form.value) return
  const key = endpointKeys.value[index]
  if (probingKeys.value.has(key)) return
  form.value.endpoints.splice(index, 1)
  endpointKeys.value.splice(index, 1)
  const nextResults = { ...probeResults.value }
  delete nextResults[key]
  probeResults.value = nextResults
}

async function probe(index: number) {
  const endpoint = form.value?.endpoints[index]
  const key = endpointKeys.value[index]
  if (!endpoint || !key || probingKeys.value.has(key)) return
  probingKeys.value = new Set(probingKeys.value).add(key)
  try {
    probeResults.value = {
      ...probeResults.value,
      [key]: await stepUp.run(() => promptAuditAPI.probeEndpoint(endpointToUpdate(endpoint)))
    }
  } catch (error) {
    showActionError(error, copy.value.probeFailed)
  } finally {
    const next = new Set(probingKeys.value)
    next.delete(key)
    probingKeys.value = next
  }
}

function currentFilter(): PromptAuditEventFilter {
  const userID = Number(filterForm.user_id)
  return {
    page: eventList.page,
    page_size: eventList.page_size,
    decision: filterForm.decision || undefined,
    risk_level: filterForm.risk_level || undefined,
    group_id: filterForm.group_id > 0 ? filterForm.group_id : undefined,
    user_id: Number.isInteger(userID) && userID > 0 ? userID : undefined,
    search: filterForm.search.trim() || undefined
  }
}

async function loadEvents() {
  eventsLoading.value = true
  try {
    const result = await promptAuditAPI.listEvents(currentFilter())
    eventList.items = result.items || []
    eventList.total = result.total
    eventList.page = result.page
    eventList.page_size = result.page_size
    selectedEventIDs.value = selectedEventIDs.value.filter((id) => eventList.items.some((event) => event.id === id))
    eventsLoaded.value = true
  } catch (error) {
    appStore.showError(safeError(error, copy.value.loadFailed))
  } finally {
    eventsLoading.value = false
  }
}

function activateEvents() {
  activeTab.value = 'events'
  if (!eventsLoaded.value) loadEvents()
}

function applyFilters() {
  eventList.page = 1
  selectedEventIDs.value = []
  loadEvents()
}

function resetFilters() {
  filterForm.decision = ''
  filterForm.risk_level = ''
  filterForm.group_id = 0
  filterForm.user_id = ''
  filterForm.search = ''
  applyFilters()
}

function changePage(page: number) { eventList.page = page; selectedEventIDs.value = []; loadEvents() }
function changePageSize(pageSize: number) { eventList.page_size = pageSize; eventList.page = 1; selectedEventIDs.value = []; loadEvents() }

function toggleCurrentPage() {
  const pageIDs = eventList.items.map((event) => event.id)
  selectedEventIDs.value = allPageSelected.value ? selectedEventIDs.value.filter((id) => !pageIDs.includes(id)) : Array.from(new Set([...selectedEventIDs.value, ...pageIDs]))
}

async function openDetail(id: number) {
  try { detailEvent.value = await promptAuditAPI.getEvent(id) }
  catch (error) { appStore.showError(safeError(error, copy.value.detailFailed)) }
}

function openSingleDelete(id: number) { pendingDelete.value = { kind: 'single', ids: [id] } }
function openBatchDelete() { if (selectedEventIDs.value.length) pendingDelete.value = { kind: 'batch', ids: [...selectedEventIDs.value] } }
function closeDeleteDialog() { if (!deleting.value) pendingDelete.value = null }

async function confirmExplicitDelete() {
  const request = pendingDelete.value
  if (!request || deleting.value) return
  deleting.value = true
  try {
    const result = request.kind === 'single'
      ? await stepUp.run(() => promptAuditAPI.deleteEvent(request.ids[0]))
      : await stepUp.run(() => promptAuditAPI.deleteEvents(request.ids))
    pendingDelete.value = null
    selectedEventIDs.value = []
    const deletedCount = typeof result.deleted === 'boolean' ? (result.deleted ? 1 : 0) : result.deleted
    appStore.showSuccess(interpolate(copy.value.deletedCount, { count: deletedCount }))
    await loadEvents()
  } catch (error) {
    showActionError(error, copy.value.deleteFailed)
  } finally {
    deleting.value = false
  }
}

async function prepareFilteredDelete() {
  if (previewingDelete.value) return
  previewingDelete.value = true
  try {
    const preview = await stepUp.run(() => promptAuditAPI.previewDelete(deleteFilter(currentFilter())))
    if (preview.count <= 0) appStore.showSuccess(copy.value.nothingToDelete)
    else deletePreview.value = preview
  } catch (error) {
    showActionError(error, copy.value.deleteFailed)
  } finally {
    previewingDelete.value = false
  }
}

function closeDeletePreview() { if (!deleting.value) deletePreview.value = null }

async function confirmFilteredDelete() {
  const preview = deletePreview.value
  if (!preview || deleting.value) return
  deleting.value = true
  try {
    const result = await stepUp.run(() => promptAuditAPI.deleteByFilter(preview.confirmation_token))
    deletePreview.value = null
    selectedEventIDs.value = []
    appStore.showSuccess(interpolate(copy.value.deletedCount, { count: result.deleted }))
    eventList.page = 1
    await loadEvents()
  } catch (error) {
    showActionError(error, copy.value.deleteFailed)
  } finally {
    deleting.value = false
  }
}

function refreshActiveTab() {
  loadRuntime()
  if (activeTab.value === 'configuration') loadConfig()
  else loadEvents()
}

function formatTime(value: string): string {
  if (!value) return '-'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '-' : date.toLocaleString()
}

function decisionLabel(value: string): string {
  return ({ pass: copy.value.pass, flag: copy.value.flag, critical: copy.value.critical, unavailable: copy.value.unavailable } as Record<string, string>)[value] || value || '-'
}

function riskLabel(value: string): string {
  return ({ low: copy.value.low, medium: copy.value.medium, high: copy.value.high, critical: copy.value.critical, unknown: copy.value.unknown } as Record<string, string>)[value] || value || '-'
}

function decisionClass(value: string): string {
  if (value === 'pass') return 'bg-green-50 text-green-700 dark:bg-green-900/30 dark:text-green-300'
  if (value === 'flag') return 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
  if (value === 'critical') return 'bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-300'
  return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
}

onMounted(async () => {
  await initialize()
  runtimeTimer = window.setInterval(loadRuntime, 15000)
})

onUnmounted(() => {
  if (runtimeTimer !== undefined) window.clearInterval(runtimeTimer)
})
</script>
