<template>
  <section
    class="rounded-xl border border-gray-200 bg-gray-50/80 p-3 dark:border-dark-600 dark:bg-dark-800/60 sm:p-4"
    :aria-label="t('admin.accounts.upstreamBilling.groupPreview.title')"
  >
    <div class="flex flex-wrap items-center justify-between gap-2.5">
      <div class="flex min-w-0 items-center gap-2.5">
        <span class="grid h-8 w-8 shrink-0 place-items-center rounded-lg bg-primary-100 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300">
          <Icon name="link" size="sm" aria-hidden="true" />
        </span>
        <h3 class="text-sm font-semibold text-gray-900 dark:text-gray-100">
          {{ t('admin.accounts.upstreamBilling.groupPreview.title') }}
        </h3>
      </div>
      <div class="flex items-center gap-2">
        <button v-if="state === 'loading'" type="button" class="btn btn-ghost btn-sm" @click="reset">
          {{ t('common.cancel') }}
        </button>
        <button
          type="button"
          class="btn btn-secondary btn-sm gap-1.5"
          :disabled="disabled || state === 'loading'"
          @click="readGroup"
        >
          <Icon name="refresh" size="xs" :class="{ 'motion-safe:animate-spin': state === 'loading' }" aria-hidden="true" />
          {{ t('admin.accounts.upstreamBilling.groupPreview.read') }}
        </button>
      </div>
    </div>

    <p v-if="statusMessage" class="mt-2.5 text-xs text-gray-600 dark:text-gray-300" role="status">
      {{ statusMessage }}
    </p>
    <div v-if="fields.length" class="mt-3 space-y-2">
      <div
        v-for="field in fields"
        :key="field.key"
        class="flex flex-wrap items-start gap-2.5 rounded-lg border border-gray-200 bg-white px-3 py-2.5 dark:border-dark-600 dark:bg-dark-900/60"
      >
        <div class="min-w-0 flex-[1_1_10rem]">
          <p class="text-xs font-medium text-gray-500 dark:text-gray-400">{{ t(field.label) }}</p>
          <p class="mt-0.5 max-h-32 overflow-y-auto whitespace-pre-wrap break-words text-sm text-gray-900 dark:text-gray-100">
            {{ field.value }}
          </p>
        </div>
        <button
          type="button"
          class="btn btn-secondary btn-sm shrink-0"
          :disabled="disabled || field.value === props[field.key]"
          @click="fillField(field.key, field.value)"
        >
          {{ field.value === props[field.key] ? t('admin.accounts.upstreamBilling.groupPreview.matches') : t(field.action) }}
        </button>
      </div>
      <p class="text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.upstreamBilling.groupPreview.saveHint') }}
      </p>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { UpstreamGroupPreview } from '@/api/admin/accounts'
import Icon from '@/components/icons/Icon.vue'
import { extractApiErrorCode } from '@/utils/apiError'

const props = defineProps<{ accountId: number; name: string; notes?: string | null; disabled?: boolean }>()
const emit = defineEmits<{ fill: [field: 'name' | 'notes', value: string] }>()
const { t } = useI18n()

const state = ref<'idle' | 'loading' | 'ready' | 'empty' | 'failed' | 'busy'>('idle')
const preview = ref<UpstreamGroupPreview>()
let pending: AbortController | undefined

const statusMessage = computed(() => {
  if (state.value === 'idle' || state.value === 'ready') return ''
  return t('admin.accounts.upstreamBilling.groupPreview.' + state.value)
})

const fields = computed(() => [
  { key: 'name' as const, value: preview.value?.group_name, label: 'admin.accounts.upstreamBilling.groupPreview.groupName', action: 'admin.accounts.upstreamBilling.groupPreview.fillName' },
  { key: 'notes' as const, value: preview.value?.group_description, label: 'admin.accounts.upstreamBilling.groupPreview.groupDescription', action: 'admin.accounts.upstreamBilling.groupPreview.fillNotes' }
].filter((field): field is typeof field & { value: string } => !!field.value))

function reset() {
  pending?.abort()
  pending = undefined
  preview.value = undefined
  state.value = 'idle'
}

async function readGroup() {
  if (props.disabled || pending) return
  const request = new AbortController()
  pending = request
  preview.value = undefined
  state.value = 'loading'
  try {
    const result = await adminAPI.accounts.previewUpstreamGroup(props.accountId, request.signal)
    if (pending !== request) return
    preview.value = result
    state.value = result.group_name || result.group_description ? 'ready' : 'empty'
  } catch (error) {
    if (pending !== request) return
    state.value = extractApiErrorCode(error) === 'UPSTREAM_GROUP_PREVIEW_BUSY' ? 'busy' : 'failed'
  } finally {
    if (pending === request) pending = undefined
  }
}

function fillField(field: 'name' | 'notes', value: string) {
  if (!props.disabled && value !== props[field]) emit('fill', field, value)
}

watch(() => props.accountId, reset, { flush: 'sync' })
onBeforeUnmount(reset)
</script>
