<template>
  <div
    class="mb-4 flex flex-col gap-3 rounded-lg border border-primary-200 bg-primary-50/90 p-3 shadow-sm backdrop-blur dark:border-primary-900/50 dark:bg-primary-950/30 sm:flex-row sm:items-center sm:justify-between"
    role="region"
    :aria-label="t('admin.accounts.bulkActions.title')"
  >
    <div class="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2" aria-live="polite">
      <span class="text-sm font-semibold text-primary-950 dark:text-primary-100">
        {{
          allResultsSelected
            ? t('admin.accounts.bulkActions.selectedAll', { count: selectedIds.length })
            : selectedIds.length > 0
              ? t('admin.accounts.bulkActions.selected', { count: selectedIds.length })
              : t('admin.accounts.bulkEdit.title')
        }}
      </span>
      <template v-if="selectedIds.length > 0">
        <button type="button" class="inline-flex items-center gap-1 text-xs font-medium text-primary-700 transition hover:text-primary-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 dark:text-primary-300 dark:hover:text-primary-100" @click="$emit('select-page')">
          <Icon name="check" size="xs" />
          {{ t('admin.accounts.bulkActions.selectCurrentPage') }}
        </button>
      </template>
      <button
        v-if="!allResultsSelected && totalResults > selectedIds.length"
        type="button"
        class="inline-flex items-center gap-1 text-xs font-semibold text-primary-700 transition hover:text-primary-950 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 disabled:cursor-wait disabled:opacity-60 dark:text-primary-300 dark:hover:text-white"
        :disabled="selectingAll"
        @click="$emit('select-all-results')"
      >
        <Icon :name="selectingAll ? 'refresh' : 'checkCircle'" size="xs" :class="selectingAll ? 'animate-spin' : ''" />
        {{
          selectingAll
            ? t('admin.accounts.bulkActions.selectingAll')
            : t('admin.accounts.bulkActions.selectAllResults', { count: totalResults })
        }}
      </button>
      <template v-if="selectedIds.length > 0">
        <span class="text-primary-300 dark:text-primary-800" aria-hidden="true">/</span>
        <button type="button" class="inline-flex items-center gap-1 text-xs font-medium text-primary-700 transition hover:text-primary-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 dark:text-primary-300 dark:hover:text-primary-100" @click="$emit('clear')">
          <Icon name="x" size="xs" />
          {{ t('admin.accounts.bulkActions.clear') }}
        </button>
      </template>
    </div>

    <div class="flex min-w-0 flex-wrap items-center gap-2 sm:justify-end">
      <template v-if="selectedIds.length > 0">
        <button type="button" class="btn btn-danger btn-sm inline-flex items-center gap-1.5" :title="t('admin.accounts.bulkActions.delete')" @click="$emit('delete')">
          <Icon name="trash" size="sm" />
          <span>{{ t('admin.accounts.bulkActions.delete') }}</span>
        </button>
        <button type="button" class="btn btn-secondary btn-sm inline-flex items-center gap-1.5" :title="t('admin.accounts.bulkActions.resetStatus')" @click="$emit('reset-status')">
          <Icon name="refresh" size="sm" />
          <span class="hidden md:inline">{{ t('admin.accounts.bulkActions.resetStatus') }}</span>
        </button>
        <button type="button" class="btn btn-secondary btn-sm inline-flex items-center gap-1.5" :title="t('admin.accounts.bulkActions.refreshToken')" @click="$emit('refresh-token')">
          <Icon name="key" size="sm" />
          <span class="hidden md:inline">{{ t('admin.accounts.bulkActions.refreshToken') }}</span>
        </button>
        <button type="button" class="btn btn-primary btn-sm inline-flex items-center gap-1.5" :title="t('admin.accounts.bulkActions.edit')" @click="$emit('edit-selected')">
          <Icon name="edit" size="sm" />
          <span class="hidden md:inline">{{ t('admin.accounts.bulkActions.edit') }}</span>
        </button>
        <div class="flex items-center gap-1">
          <button type="button" class="btn btn-success btn-sm px-2" :title="t('admin.accounts.bulkActions.enableScheduling')" @click="$emit('toggle-schedulable', true)">
            <Icon name="play" size="sm" />
            <span class="sr-only">{{ t('admin.accounts.bulkActions.enableScheduling') }}</span>
          </button>
          <button type="button" class="btn btn-warning btn-sm px-2" :title="t('admin.accounts.bulkActions.disableScheduling')" @click="$emit('toggle-schedulable', false)">
            <Icon name="ban" size="sm" />
            <span class="sr-only">{{ t('admin.accounts.bulkActions.disableScheduling') }}</span>
          </button>
        </div>
      </template>
      <button type="button" class="btn btn-primary btn-sm inline-flex items-center gap-1.5" :title="t('admin.accounts.bulkEdit.submit')" @click="$emit('edit-filtered')">
        <Icon name="filter" size="sm" />
        <span>{{ t('admin.accounts.bulkEdit.submit') }}</span>
      </button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

defineProps<{
  selectedIds: number[]
  totalResults: number
  selectingAll: boolean
  allResultsSelected: boolean
}>()

defineEmits<{
  delete: []
  'edit-selected': []
  'edit-filtered': []
  clear: []
  'select-page': []
  'select-all-results': []
  'toggle-schedulable': [enabled: boolean]
  'reset-status': []
  'refresh-token': []
}>()

const { t } = useI18n()
</script>
