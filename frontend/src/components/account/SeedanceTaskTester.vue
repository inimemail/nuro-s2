<template>
  <details class="rounded-xl border border-primary-200 bg-primary-50/50 dark:border-primary-800 dark:bg-primary-950/20">
    <summary class="cursor-pointer px-4 py-3 text-sm font-semibold text-primary-800 dark:text-primary-200">{{ t('admin.accounts.openai.seedanceTestTitle') }}</summary>
    <div class="space-y-3 border-t border-primary-200/60 p-4 dark:border-primary-800">
      <p class="text-xs leading-5 text-gray-600 dark:text-gray-300">{{ t('admin.accounts.openai.seedanceTestHint') }}</p>
      <label class="block text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.accounts.openai.seedanceDownstreamKey') }}
        <input v-model="apiKey" type="password" autocomplete="off" :disabled="busy || submitted" class="input mt-1 w-full" placeholder="sk-…" />
      </label>
      <label class="block text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.accounts.selectTestModel') }}
        <input v-model="requestModel" :disabled="busy || submitted" class="input mt-1 w-full" placeholder="doubao-seedance-…" />
      </label>
      <textarea v-model="prompt" :disabled="busy || submitted" class="input min-h-20 w-full resize-y" :placeholder="t('admin.accounts.imagePromptLabel')" />
      <label class="flex items-start gap-2 text-xs leading-5 text-amber-800 dark:text-amber-300">
        <input v-model="consent" type="checkbox" :disabled="busy || submitted" class="mt-1 rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
        {{ t('admin.accounts.openai.seedanceCostConsent') }}
      </label>
      <div class="flex flex-wrap gap-2">
        <button type="button" class="btn btn-primary" :disabled="busy || submitted || !consent || !apiKey.trim() || !requestModel.trim() || !prompt.trim()" @click="createTask">{{ t('admin.accounts.openai.seedanceCreateTask') }}</button>
        <button v-if="submitted && !operationID && !terminal" type="button" class="btn btn-secondary" :disabled="busy" @click="call('POST')">{{ t('admin.accounts.openai.seedanceRecoverSubmission') }}</button>
        <button v-if="operationID" type="button" class="btn btn-secondary" :disabled="busy" @click="queryTask">{{ t('common.refresh') }}</button>
        <button v-if="providerID && !terminal" type="button" class="btn btn-secondary" :disabled="busy" @click="cancelTask">{{ t('admin.accounts.openai.seedanceCancelTask') }}</button>
        <button v-if="terminal" type="button" class="btn btn-secondary" :disabled="busy" @click="resetTask">{{ t('admin.accounts.openai.seedanceNewTask') }}</button>
      </div>
      <div v-if="operationID" class="rounded-lg border border-gray-200 bg-white p-3 text-xs dark:border-dark-600 dark:bg-dark-900">
        <p class="font-medium text-gray-700 dark:text-gray-200">{{ taskStatus || t('common.loading') }}</p>
        <code class="mt-1 block select-all break-all text-gray-500">{{ providerID || operationID }}</code>
        <code v-if="providerID && operationID !== providerID" class="mt-1 block select-all break-all text-gray-400">{{ operationID }}</code>
        <p class="mt-2 text-gray-500">{{ t('admin.accounts.openai.seedanceTaskPersistence') }}</p>
      </div>
      <p v-if="message" role="status" class="break-words text-xs leading-5 text-amber-700 dark:text-amber-300">{{ message }}</p>
      <a v-if="videoURL" :href="videoURL" target="_blank" rel="noopener noreferrer" class="inline-flex text-sm font-medium text-primary-600 hover:underline dark:text-primary-400">{{ t('admin.accounts.videoPreview') }} ↗</a>
    </div>
  </details>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { buildGatewayUrl } from '@/api/url'

const props = defineProps<{ model?: string }>()
const { t } = useI18n()
const apiKey = ref('')
const requestModel = ref(props.model || '')
const prompt = ref('')
const consent = ref(false)
const busy = ref(false)
const submitted = ref(false)
const operationID = ref('')
const providerID = ref('')
const taskStatus = ref('')
const message = ref('')
const videoURL = ref('')
const terminal = computed(() => ['succeeded', 'failed', 'cancelled', 'expired', 'rejected'].includes(taskStatus.value))
let controller: AbortController | undefined
let timer: ReturnType<typeof setTimeout> | undefined
let polls = 0
let disposed = false
let idempotencyKey = ''

watch(() => props.model, (value) => { if (!submitted.value && !busy.value) requestModel.value = value || '' })

async function call(method: 'POST' | 'GET' | 'DELETE') {
  if (busy.value || disposed) return
  clearTimeout(timer)
  busy.value = true
  message.value = ''
  controller = new AbortController()
  const timeout = setTimeout(() => controller?.abort(), method === 'POST' ? 150_000 : 35_000)
  try {
    const headers: Record<string, string> = { Authorization: `Bearer ${apiKey.value.trim()}`, 'Content-Type': 'application/json' }
    if (method === 'POST') {
      idempotencyKey ||= crypto.randomUUID()
      headers['Idempotency-Key'] = idempotencyKey
      submitted.value = true
    }
    const response = await fetch(buildGatewayUrl(`/api/v3/contents/generations/tasks${method === 'POST' ? '' : `/${encodeURIComponent(operationID.value || providerID.value)}`}`), {
      method, headers, signal: controller.signal,
      body: method === 'POST' ? JSON.stringify({ model: requestModel.value.trim(), content: [{ type: 'text', text: prompt.value.trim() }] }) : undefined
    })
    const localID = response.headers.get('X-Seedance-Operation-ID')
    if (localID) operationID.value = localID
    const data = response.status === 204 ? {} : await response.json()
    if (disposed) return
    if (!response.ok) {
      if (!operationID.value && response.status >= 400 && response.status < 500 && ![408, 409].includes(response.status)) taskStatus.value = 'rejected'
      throw new Error(data.error?.message || `HTTP ${response.status}`)
    }
    if (method !== 'DELETE' && typeof data.id === 'string' && data.id !== operationID.value) providerID.value = data.id
    if (!operationID.value && providerID.value) operationID.value = providerID.value
    taskStatus.value = data.status || (method === 'POST' ? 'queued' : taskStatus.value)
    if (data.error?.message) message.value = String(data.error.message)
    const url = data.content?.video_url
    if (typeof url === 'string' && /^https?:\/\//i.test(url)) videoURL.value = url
    if (method === 'DELETE') message.value = t('admin.accounts.openai.seedanceCancellationPending')
  } catch (error) {
    if (!disposed) message.value = error instanceof Error ? error.message : t('common.error')
  } finally {
    clearTimeout(timeout)
    busy.value = false
    if (!disposed && operationID.value && !terminal.value && ++polls < 120) timer = setTimeout(queryTask, 5000)
  }
}

function createTask() { if (!operationID.value && consent.value && apiKey.value.trim() && requestModel.value.trim() && prompt.value.trim()) void call('POST') }
function queryTask() { if (operationID.value) void call('GET') }
function cancelTask() { if (providerID.value) void call('DELETE') }
function resetTask() { if (!terminal.value || busy.value) return; clearTimeout(timer); operationID.value = ''; providerID.value = ''; taskStatus.value = ''; videoURL.value = ''; message.value = ''; consent.value = false; submitted.value = false; idempotencyKey = ''; polls = 0 }
onBeforeUnmount(() => { disposed = true; clearTimeout(timer); controller?.abort(); apiKey.value = '' })
</script>
