<template>
  <BaseDialog :show="Boolean(event)" :title="copy.details" width="wide" @close="emit('close')">
    <div v-if="event" class="space-y-5">
      <div class="rounded-md border border-sky-200 bg-sky-50 p-3 text-sm text-sky-800 dark:border-sky-900/60 dark:bg-sky-900/20 dark:text-sky-200">
        <div class="flex items-start gap-2">
          <Icon name="shield" size="sm" class="mt-0.5 flex-shrink-0" />
          <p>{{ copy.noPromptNotice }}</p>
        </div>
      </div>

      <dl class="grid grid-cols-1 gap-x-6 gap-y-4 text-sm md:grid-cols-2">
        <DetailField :label="copy.time" :value="formatTime(event.created_at)" />
        <DetailField :label="copy.requestId" :value="event.request_id || '-'" mono />
        <DetailField :label="copy.identity" :value="identity" />
        <DetailField :label="copy.group" :value="event.group_name || '-'" />
        <DetailField :label="copy.providerProtocol" :value="`${event.provider || '-'} / ${event.protocol || '-'}`" />
        <DetailField :label="copy.route" :value="`${event.endpoint || '-'} / ${event.model || '-'}`" mono />
        <DetailField :label="copy.decision" :value="event.decision || '-'" />
        <DetailField :label="copy.risk" :value="event.risk_level || '-'" />
        <DetailField :label="copy.stage" :value="event.stage || '-'" />
        <DetailField :label="copy.latency" :value="`${event.latency_ms} ms`" />
        <DetailField :label="copy.promptLength" :value="String(event.prompt_length)" />
        <DetailField :label="copy.messageCount" :value="String(event.message_count)" />
        <DetailField :label="copy.scannerBackend" :value="`${event.scanner_backend || '-'} / ${event.scanner_version || '-'}`" mono />
        <DetailField :label="copy.guardEndpoint" :value="event.guard_endpoint_id || '-'" mono />
        <DetailField class="md:col-span-2" :label="copy.categories" :value="event.categories?.join(', ') || '-'" />
        <DetailField v-if="safeErrorCode" class="md:col-span-2" :label="copy.errorCode" :value="safeErrorCode" mono />
        <DetailField class="md:col-span-2" :label="copy.promptHash" :value="event.prompt_hash || '-'" mono break-all />
        <div class="md:col-span-2">
          <dt class="text-gray-500 dark:text-gray-400">{{ copy.preview }}</dt>
          <dd class="mt-1 whitespace-pre-wrap break-words rounded-md bg-gray-50 p-3 text-gray-800 dark:bg-dark-900 dark:text-gray-200">{{ event.redacted_preview || '-' }}</dd>
        </div>
      </dl>
    </div>
    <template #footer>
      <button type="button" class="btn btn-secondary" @click="emit('close')">{{ copy.close }}</button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, defineComponent, h } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import type { PromptAuditCopy } from '../copy'
import type { PromptAuditEvent } from '../types'

const props = defineProps<{ event: PromptAuditEvent | null; copy: PromptAuditCopy }>()
const emit = defineEmits<{ (event: 'close'): void }>()

const identity = computed(() => {
  if (!props.event) return '-'
  const user = props.event.user_email || (props.event.user_id ? `UID ${props.event.user_id}` : '-')
  const key = props.event.api_key_name || (props.event.api_key_id ? `Key #${props.event.api_key_id}` : '-')
  return `${user} / ${key}`
})

const safeErrorCode = computed(() => {
  const value = props.event?.error_code || ''
  return /^[a-z0-9_]{1,80}$/i.test(value) ? value : ''
})

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '-' : date.toLocaleString()
}

const DetailField = defineComponent({
  props: {
    label: { type: String, required: true },
    value: { type: String, required: true },
    mono: { type: Boolean, default: false },
    breakAll: { type: Boolean, default: false }
  },
  setup(fieldProps) {
    return () => h('div', [
      h('dt', { class: 'text-gray-500 dark:text-gray-400' }, fieldProps.label),
      h('dd', {
        class: ['mt-1 text-gray-900 dark:text-white', fieldProps.mono && 'font-mono text-xs', fieldProps.breakAll && 'break-all']
      }, fieldProps.value)
    ])
  }
})
</script>
