<template>
  <BaseDialog :show="show" :title="t('admin.users.bulkLimits.title')" width="normal" @close="emit('close')">
    <form id="bulk-edit-user-limits-form" class="space-y-5" @submit.prevent="handleSubmit">
      <fieldset class="space-y-2">
        <legend class="input-label">{{ t('admin.users.bulkLimits.scope') }}</legend>
        <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
          <input v-model="scope" type="radio" value="selected" data-test="scope-selected" :disabled="selectedIds.length === 0" class="text-primary-600 focus:ring-primary-500 disabled:opacity-50" />
          {{ t('admin.users.bulkLimits.selectedCount', { count: selectedIds.length }) }}
        </label>
        <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
          <input v-model="scope" type="radio" value="all" data-test="scope-all" class="text-primary-600 focus:ring-primary-500" />
          {{ t('admin.users.bulkLimits.allUsers') }}
        </label>
      </fieldset>
      <div class="divide-y divide-gray-200 border-y border-gray-200 dark:divide-dark-700 dark:border-dark-700">
        <div class="space-y-3 py-4">
          <div class="flex items-center justify-between gap-4">
            <label for="bulk-concurrency" class="input-label mb-0">{{ t('admin.users.columns.concurrency') }}</label>
            <Toggle v-model="enableConcurrency" :aria-label="t('admin.users.bulkLimits.enableConcurrency')" data-test="enable-concurrency" />
          </div>
          <input v-if="enableConcurrency" id="bulk-concurrency" v-model="concurrencyValue" type="number" min="0" step="1" class="input" data-test="concurrency-input" />
        </div>
        <div class="space-y-3 py-4">
          <div class="flex items-center justify-between gap-4">
            <label for="bulk-rpm-limit" class="input-label mb-0">{{ t('admin.users.form.rpmLimit') }}</label>
            <Toggle v-model="enableRPMLimit" :aria-label="t('admin.users.bulkLimits.enableRPMLimit')" data-test="enable-rpm-limit" />
          </div>
          <div v-if="enableRPMLimit">
            <input id="bulk-rpm-limit" v-model="rpmLimitValue" type="number" min="0" step="1" class="input" data-test="rpm-limit-input" />
            <p v-if="parsedRPMLimit === 0" class="input-hint">{{ t('admin.users.bulkLimits.unlimited') }}</p>
          </div>
        </div>
      </div>
      <p v-if="hasInvalidValue" class="text-sm text-red-600 dark:text-red-400">{{ t('admin.users.bulkLimits.nonNegativeInteger') }}</p>
      <p v-if="selectionTooLarge" class="text-sm text-red-600 dark:text-red-400">{{ t('admin.users.bulkLimits.selectionLimit', { max: MAX_BATCH_USER_IDS }) }}</p>
    </form>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" @click="emit('close')">{{ t('common.cancel') }}</button>
        <button type="submit" form="bulk-edit-user-limits-form" class="btn btn-primary" data-test="submit" :disabled="!canSubmit">{{ submitting ? t('admin.users.bulkLimits.applying') : t('admin.users.bulkLimits.apply') }}</button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { BatchUpdateUserLimitsRequest } from '@/api/admin/users'
import { useAppStore } from '@/stores/app'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Toggle from '@/components/common/Toggle.vue'

const props = defineProps<{ show: boolean; selectedIds: number[] }>()
const emit = defineEmits<{ close: []; success: [affected: number] }>()
const { t } = useI18n()
const appStore = useAppStore()
const enableConcurrency = ref(false)
const enableRPMLimit = ref(false)
const concurrencyValue = ref<string | number>('')
const rpmLimitValue = ref<string | number>('')
const submitting = ref(false)
const scope = ref<'selected' | 'all'>(props.selectedIds.length > 0 ? 'selected' : 'all')
const MAX_BATCH_USER_IDS = 500
const parseLimit = (value: string | number): number | null | undefined => {
  const text = String(value).trim()
  if (!text) return undefined
  const parsed = Number(text)
  return Number.isInteger(parsed) && parsed >= 0 ? parsed : null
}
const parsedConcurrency = computed(() => enableConcurrency.value ? parseLimit(concurrencyValue.value) : undefined)
const parsedRPMLimit = computed(() => enableRPMLimit.value ? parseLimit(rpmLimitValue.value) : undefined)
const hasInvalidValue = computed(() => parsedConcurrency.value === null || parsedRPMLimit.value === null)
const hasUpdate = computed(() => parsedConcurrency.value != null || parsedRPMLimit.value != null)
const selectionTooLarge = computed(() => scope.value === 'selected' && props.selectedIds.length > MAX_BATCH_USER_IDS)
const hasTarget = computed(() => scope.value === 'all' || props.selectedIds.length > 0)
const canSubmit = computed(() => hasTarget.value && !selectionTooLarge.value && hasUpdate.value && !hasInvalidValue.value && !submitting.value)
watch(() => props.show, show => {
  if (!show) return
  scope.value = props.selectedIds.length > 0 ? 'selected' : 'all'
  enableConcurrency.value = false
  enableRPMLimit.value = false
  concurrencyValue.value = ''
  rpmLimitValue.value = ''
})
const handleSubmit = async () => {
  if (!canSubmit.value) return
  const applyToAll = scope.value === 'all'
  const request: BatchUpdateUserLimitsRequest = { user_ids: applyToAll ? [] : [...props.selectedIds], all: applyToAll }
  if (parsedConcurrency.value != null) request.concurrency = parsedConcurrency.value
  if (parsedRPMLimit.value != null) request.rpm_limit = parsedRPMLimit.value
  const confirmationKey = applyToAll ? 'admin.users.bulkLimits.confirmAll' : 'admin.users.bulkLimits.confirm'
  if (!window.confirm(t(confirmationKey, { count: props.selectedIds.length }))) return
  submitting.value = true
  try {
    const result = await adminAPI.users.batchUpdateLimits(request)
    appStore.showSuccess(t('admin.users.bulkLimits.success', { count: result.affected }))
    emit('success', result.affected)
    emit('close')
  } catch (error: any) {
    appStore.showError(error.response?.data?.message || t('admin.users.bulkLimits.failed'))
  } finally {
    submitting.value = false
  }
}
</script>
