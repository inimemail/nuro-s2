<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.testAccountConnection')"
    width="normal"
    @close="handleClose"
  >
    <div class="space-y-4">
      <!-- Account Info Card -->
      <div
        v-if="account"
        class="flex items-center justify-between rounded-xl border border-gray-200 bg-gradient-to-r from-gray-50 to-gray-100 p-3 dark:border-dark-500 dark:from-dark-700 dark:to-dark-600"
      >
        <div class="flex min-w-0 items-center gap-3">
          <div
            class="flex h-10 w-10 items-center justify-center rounded-lg bg-gradient-to-br from-primary-500 to-primary-600"
          >
            <Icon name="play" size="md" class="text-white" :stroke-width="2" />
          </div>
          <div>
            <div class="break-words font-semibold text-gray-900 dark:text-gray-100">{{ account.name }}</div>
            <div class="flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
              <span
                class="rounded bg-gray-200 px-1.5 py-0.5 text-[10px] font-medium uppercase dark:bg-dark-500"
              >
                {{ account.type }}
              </span>
              <span>{{ t('admin.accounts.account') }}</span>
            </div>
          </div>
        </div>
        <span
          :class="[
            'rounded-full px-2.5 py-1 text-xs font-semibold',
            account.status === 'active'
              ? 'bg-green-100 text-green-700 dark:bg-green-500/20 dark:text-green-400'
              : 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-400'
          ]"
        >
          {{ account.status }}
        </span>
      </div>

      <SeedanceTaskTester v-if="show && account?.platform === 'openai' && account.type === 'apikey' && isSeedanceEnabled(account.credentials)" :model="selectedModelId" />
      <div v-if="grokNeedsModel" class="space-y-1.5">
        <label class="text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.accounts.selectTestModel') }}
        </label>
        <Select
          v-model="selectedModelId"
          :options="testModelOptions"
          :disabled="loadingModels || status === 'connecting'"
          value-key="id"
          label-key="display_name"
          :placeholder="loadingModels ? t('common.loading') + '...' : t('admin.accounts.selectTestModel')"
        />
      </div>

      <div v-if="modelLoadError" role="alert" class="flex items-start gap-3 rounded-xl border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-300">
        <Icon name="exclamationTriangle" size="sm" class="mt-0.5 shrink-0" />
        <p class="min-w-0 flex-1 break-words">{{ modelLoadError }}</p>
        <button type="button" class="shrink-0 rounded-lg px-2 py-1 font-medium hover:bg-red-100 dark:hover:bg-red-900/40" @click="loadAvailableModels">{{ t('common.retry') }}</button>
      </div>

      <div v-if="isOpenAIAccount" class="space-y-1.5">
        <label class="text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.accounts.openai.testMode') }}
        </label>
        <Select
          v-model="testMode"
          :options="openAITestModeOptions"
          :disabled="status === 'connecting'"
        />
      </div>

      <div v-if="account?.platform === 'grok'" class="space-y-1.5">
        <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.accounts.grokTestMode') }}</label>
        <Select v-model="grokTestMode" :options="grokTestModeOptions" :disabled="status === 'connecting'" />
      </div>

      <div v-if="account?.platform === 'grok' && ['image', 'video', 'stt'].includes(grokTestMode)" class="space-y-1.5">
        <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t(grokTestMode === 'video' ? 'admin.accounts.grokUploadFirstFrame' : 'admin.accounts.grokUploadMedia') }}</label>
        <input :key="grokTestMode" type="file" :accept="grokTestMode === 'stt' ? 'audio/*' : 'image/*'" :disabled="status === 'connecting'" class="block w-full rounded-lg border border-gray-200 px-3 py-2 text-sm file:mr-3 file:rounded-md file:border-0 file:bg-primary-50 file:px-3 file:py-1 file:text-primary-700 dark:border-dark-500 dark:bg-dark-700 dark:file:bg-dark-600 dark:file:text-primary-300" @change="handleGrokMediaUpload" />
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ grokMediaName || t(grokTestMode === 'stt' ? 'admin.accounts.grokAudioRequired' : 'admin.accounts.grokMediaOptional') }}</p>
      </div>

      <div v-if="supportsImageTest || grokSupportsPrompt" class="space-y-1.5">
        <TextArea
          v-model="testPrompt"
          :label="t(isGrokAccount ? 'admin.accounts.grokPromptLabel' : 'admin.accounts.imagePromptLabel')"
          :placeholder="t(isGrokAccount ? 'admin.accounts.grokPromptPlaceholder' : 'admin.accounts.imagePromptPlaceholder')"
          :hint="t(isGrokAccount ? 'admin.accounts.grokDirectTestHint' : 'admin.accounts.imageTestHint')"
          :disabled="status === 'connecting'"
          rows="3"
        />
      </div>

      <!-- Terminal Output -->
      <div class="group relative">
        <div
          ref="terminalRef"
          class="max-h-[240px] min-h-[120px] overflow-y-auto rounded-xl border border-gray-700 bg-gray-900 p-4 font-mono text-sm dark:border-gray-800 dark:bg-black"
        >
          <!-- Status Line -->
          <div v-if="status === 'idle'" class="flex items-center gap-2 text-gray-500">
            <Icon name="play" size="sm" :stroke-width="2" />
            <span>{{ t('admin.accounts.readyToTest') }}</span>
          </div>
          <div v-else-if="status === 'connecting'" class="flex items-center gap-2 text-yellow-400">
            <Icon name="refresh" size="sm" class="animate-spin" :stroke-width="2" />
            <span>{{ t('admin.accounts.connectingToApi') }}</span>
          </div>

          <!-- Output Lines -->
          <div v-for="(line, index) in outputLines" :key="index" :class="line.class">
            {{ line.text }}
          </div>

          <!-- Streaming Content -->
          <div v-if="streamingContent" class="text-green-400">
            {{ streamingContent }}<span class="animate-pulse">_</span>
          </div>

          <!-- Result Status -->
          <div
            v-if="status === 'success'"
            class="mt-3 flex items-center gap-2 border-t border-gray-700 pt-3 text-green-400"
          >
            <Icon name="check" size="sm" :stroke-width="2" />
            <span>{{ t('admin.accounts.testCompleted') }}</span>
          </div>
          <div
            v-else-if="status === 'error'"
            class="mt-3 flex items-center gap-2 border-t border-gray-700 pt-3 text-red-400"
          >
            <Icon name="x" size="sm" :stroke-width="2" />
            <span>{{ errorMessage }}</span>
          </div>
        </div>

        <!-- Copy Button -->
        <button
          v-if="outputLines.length > 0"
          @click="copyOutput"
          class="absolute right-2 top-2 rounded-lg bg-gray-800/80 p-1.5 text-gray-400 opacity-0 transition-all hover:bg-gray-700 hover:text-white group-hover:opacity-100"
          :title="t('admin.accounts.copyOutput')"
        >
          <Icon name="link" size="sm" :stroke-width="2" />
        </button>
      </div>

      <div v-if="generatedImages.length > 0" class="space-y-2">
        <div class="text-xs font-medium text-gray-600 dark:text-gray-300">
          {{ t('admin.accounts.imagePreview') }}
        </div>
        <div class="flex flex-wrap justify-center gap-3">
          <div
            v-for="(image, index) in generatedImages"
            :key="`${image.url}-${index}`"
            class="group/img relative cursor-pointer overflow-hidden rounded-xl border border-gray-200 bg-white shadow-sm transition hover:border-primary-300 hover:shadow-md dark:border-dark-500 dark:bg-dark-700"
            @click="previewImageUrl = image.url"
          >
            <img :src="image.url" :alt="`test-image-${index + 1}`" class="max-h-[360px] w-full object-contain" />
            <div class="absolute inset-0 flex items-center justify-center bg-black/0 transition-colors group-hover/img:bg-black/20">
              <Icon name="eye" size="lg" class="text-white opacity-0 drop-shadow-lg transition-opacity group-hover/img:opacity-100" :stroke-width="2" />
            </div>
            <div class="border-t border-gray-100 px-3 py-1.5 text-xs text-gray-500 dark:border-dark-500 dark:text-gray-300">
              {{ image.mimeType || 'image/*' }}
            </div>
          </div>
        </div>
      </div>

      <div v-if="generatedVideos.length > 0" class="space-y-2">
        <div class="text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('admin.accounts.videoPreview') }}</div>
        <video v-for="(video, index) in generatedVideos" :key="`${video}-${index}`" :src="video" controls class="max-h-[360px] w-full rounded-xl border border-gray-200 dark:border-dark-500" />
      </div>
      <div v-if="generatedAudio.length > 0" class="space-y-2">
        <div class="text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('admin.accounts.audioPreview') }}</div>
        <audio v-for="(audio, index) in generatedAudio" :key="`${audio}-${index}`" :src="audio" controls class="w-full" />
      </div>

      <!-- Image Lightbox -->
      <Teleport to="body">
        <Transition name="fade">
          <div
            v-if="previewImageUrl"
            class="fixed inset-0 z-[100] flex items-center justify-center bg-black/80 p-4"
            @click.self="previewImageUrl = ''"
          >
            <button
              class="absolute right-4 top-4 rounded-full bg-black/50 p-2 text-white transition-colors hover:bg-black/70"
              @click="previewImageUrl = ''"
            >
              <Icon name="x" size="lg" :stroke-width="2" />
            </button>
            <img
              :src="previewImageUrl"
              alt="preview"
              class="max-h-[90vh] max-w-[90vw] rounded-lg object-contain shadow-2xl"
            />
          </div>
        </Transition>
      </Teleport>

      <!-- Test Info -->
      <div class="flex items-center justify-between px-1 text-xs text-gray-500 dark:text-gray-400">
        <div class="flex items-center gap-3">
          <span class="flex items-center gap-1">
            <Icon name="grid" size="sm" :stroke-width="2" />
            {{ t('admin.accounts.testModel') }}
          </span>
        </div>
        <span class="flex items-center gap-1">
          <Icon name="chat" size="sm" :stroke-width="2" />
          {{
            supportsImageTest
              ? t('admin.accounts.imageTestMode')
              : t('admin.accounts.testPrompt')
          }}
        </span>
      </div>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button
          @click="handleClose"
          class="rounded-lg bg-gray-100 px-4 py-2 text-sm font-medium text-gray-700 transition-colors hover:bg-gray-200 dark:bg-dark-600 dark:text-gray-300 dark:hover:bg-dark-500"
        >
          {{ t('common.close') }}
        </button>
        <button
          @click="startTest"
          :disabled="status === 'connecting' || loadingModels || !grokCanStart"
          :class="[
            'flex items-center gap-2 rounded-lg px-4 py-2 text-sm font-medium transition-all',
            status === 'connecting' || loadingModels || !grokCanStart
              ? 'cursor-not-allowed bg-primary-400 text-white'
              : status === 'success'
                ? 'bg-green-500 text-white hover:bg-green-600'
                : status === 'error'
                  ? 'bg-orange-500 text-white hover:bg-orange-600'
                  : 'bg-primary-500 text-white hover:bg-primary-600'
          ]"
        >
          <Icon
            v-if="status === 'connecting'"
            name="refresh"
            size="sm"
            class="animate-spin"
            :stroke-width="2"
          />
          <Icon v-else-if="status === 'idle'" name="play" size="sm" :stroke-width="2" />
          <Icon v-else name="refresh" size="sm" :stroke-width="2" />
          <span>
            {{
              status === 'connecting'
                ? t('admin.accounts.testing')
                : status === 'idle'
                  ? t('admin.accounts.startTest')
                  : t('admin.accounts.retry')
            }}
          </span>
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import SeedanceTaskTester from './SeedanceTaskTester.vue'
import { isSeedanceEnabled } from '@/utils/seedance'
import { computed, ref, watch, nextTick, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import TextArea from '@/components/common/TextArea.vue'
import { Icon } from '@/components/icons'
import { useClipboard } from '@/composables/useClipboard'
import { adminAPI } from '@/api/admin'
import { useGrokAccountTest, type AccountTestModel } from '@/composables/useGrokAccountTest'
import { extractApiErrorMessage } from '@/utils/apiError'
import type { Account } from '@/types'
import { sortAccountTestModels } from '@/utils/accountTestModels'

const { t } = useI18n()
const { copyToClipboard } = useClipboard()

interface OutputLine {
  text: string
  class: string
}

interface PreviewImage {
  url: string
  mimeType?: string
}

const props = defineProps<{
  show: boolean
  account: Account | null
}>()

const emit = defineEmits<{
  (e: 'close'): void
}>()

const terminalRef = ref<HTMLElement | null>(null)
const status = ref<'idle' | 'connecting' | 'success' | 'error'>('idle')
const outputLines = ref<OutputLine[]>([])
const streamingContent = ref('')
const errorMessage = ref('')
const availableModels = ref<AccountTestModel[]>([])
const selectedModelId = ref('')
const testPrompt = ref('')
const loadingModels = ref(false)
const modelLoadError = ref('')
let modelGeneration = 0
let streamGeneration = 0
let disposed = false
let abortController: AbortController | null = null
const generatedImages = ref<PreviewImage[]>([])
const generatedVideos = ref<string[]>([])
const generatedAudio = ref<string[]>([])
const testMode = ref<'default' | 'compact' | 'compact_legacy'>('default')
const isGrokAccount = computed(() => props.account?.platform === 'grok')
const {
  mode: grokTestMode, options: testModelOptions, needsModel: grokNeedsModel,
  supportsPrompt: grokSupportsPrompt, canStart: grokCanStart, mediaName: grokMediaName,
  clearMedia: clearGrokMedia, upload: handleGrokMediaUpload,
  requestFields: grokRequestFields, requestModel: grokRequestModel
} = useGrokAccountTest(isGrokAccount, availableModels, selectedModelId, testPrompt,
  key => addLine(t(key), 'text-red-400'))
const grokTestModeOptions = computed(() => [
  { value: 'text', label: t('admin.accounts.grokModes.responses') }, { value: 'chat', label: t('admin.accounts.grokModes.chat') },
  ...(props.account?.grok_media_eligible === false ? [] : [{ value: 'image', label: t('admin.accounts.grokModes.image') }, { value: 'video', label: t('admin.accounts.grokModes.video') }]),
  { value: 'search', label: t('admin.accounts.grokModes.search') }, { value: 'tts', label: t('admin.accounts.grokModes.tts') },
  { value: 'stt', label: t('admin.accounts.grokModes.stt') }, { value: 'realtime', label: t('admin.accounts.grokModes.realtime') },
])
const isOpenAIAccount = computed(() => props.account?.platform === 'openai')
const openAITestModeOptions = computed(() => [
  { value: 'default', label: t('admin.accounts.openai.testModeDefault') },
  { value: 'compact', label: t('admin.accounts.openai.testModeCompact') },
  { value: 'compact_legacy', label: t('admin.accounts.openai.testModeCompactLegacy') }
])
const previewImageUrl = ref('')
const supportsGeminiImageTest = computed(() => {
  const modelID = selectedModelId.value.toLowerCase()
  if (!modelID.startsWith('gemini-') || !modelID.includes('-image')) return false

  return props.account?.platform === 'gemini' || (props.account?.platform === 'antigravity' && props.account?.type === 'apikey')
})

const supportsOpenAIImageTest = computed(() => {
  const modelID = selectedModelId.value.toLowerCase()
  if (modelID.startsWith('gemini-') && (modelID.endsWith('-image') || modelID.includes('-image-'))) return props.account?.platform === 'openai' && props.account?.type === 'apikey'
  if (!modelID.startsWith('gpt-image-')) return false
  return props.account?.platform === 'openai'
})

const supportsImageTest = computed(() => supportsGeminiImageTest.value || supportsOpenAIImageTest.value)

watch(selectedModelId, () => {
  if (supportsImageTest.value && !testPrompt.value.trim()) {
    testPrompt.value = t('admin.accounts.imagePromptDefault')
  }
})

const loadAvailableModels = async () => {
  if (!props.account) return

  const account = props.account
  const generation = ++modelGeneration
  const current = () => !disposed && props.show && props.account?.id === account.id && generation === modelGeneration
  const previousSelection = selectedModelId.value
  loadingModels.value = true
  modelLoadError.value = ''
  try {
    const models = await adminAPI.accounts.getAvailableModels(account.id)
    if (!current()) return
    availableModels.value = sortAccountTestModels(models, account.platform)
    if (availableModels.value.some(m => m.id === previousSelection)) {
      selectedModelId.value = previousSelection
      return
    }
    // Default selection by platform
    if (availableModels.value.length > 0) {
      if (props.account.platform === 'grok') {
        selectedModelId.value = testModelOptions.value[0]?.id || ''
      } else if (props.account.platform === 'gemini') {
        selectedModelId.value = availableModels.value[0].id
      } else {
        // Try to select Sonnet as default, otherwise use first model
        const sonnetModel = availableModels.value.find((m) => m.id.includes('sonnet'))
        selectedModelId.value = sonnetModel?.id || availableModels.value[0].id
      }
    }
  } catch (error) {
    if (!current()) return
    modelLoadError.value = extractApiErrorMessage(error, t('common.error'))
    console.error('Failed to load available models:', error)
    // Fallback to empty list
    availableModels.value = []
    selectedModelId.value = ''
  } finally {
    if (current()) loadingModels.value = false
  }
}

const resetState = () => {
  status.value = 'idle'
  outputLines.value = []
  streamingContent.value = ''
  errorMessage.value = ''
  generatedImages.value = []
  generatedVideos.value = []
  generatedAudio.value = []
  previewImageUrl.value = ''
}

const handleClose = () => {
  modelGeneration++
  abortStream()
  emit('close')
}

const abortStream = () => {
  streamGeneration++
  if (abortController) {
    abortController.abort()
    abortController = null
  }
}

const addLine = (text: string, className: string = 'text-gray-300') => {
  outputLines.value.push({ text, class: className })
  if (outputLines.value.length > 2000) outputLines.value.splice(0, outputLines.value.length - 2000)
  scrollToBottom()
}

const scrollToBottom = async () => {
  await nextTick()
  if (terminalRef.value) {
    terminalRef.value.scrollTop = terminalRef.value.scrollHeight
  }
}

const startTest = async () => {
  if (!props.account || loadingModels.value || !grokCanStart.value || status.value === 'connecting') return

  resetState()
  status.value = 'connecting'
  addLine(t('admin.accounts.startingTestForAccount', { name: props.account.name }), 'text-blue-400')
  addLine(t('admin.accounts.testAccountTypeLabel', { type: props.account.type }), 'text-gray-400')
  addLine('', 'text-gray-300')

  abortStream()

  const controller = new AbortController()
  abortController = controller
  const generation = streamGeneration
  const accountID = props.account.id
  const current = () => !disposed && props.show && props.account?.id === accountID && generation === streamGeneration

  try {
    // Create EventSource for SSE
    const url = `/api/v1/admin/accounts/${props.account.id}/test`

    // Use fetch with streaming for SSE since EventSource doesn't support POST
    const response = await fetch(url, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${localStorage.getItem('auth_token')}`,
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({
        ...grokRequestFields.value,
        model_id: isGrokAccount.value ? grokRequestModel.value : selectedModelId.value,
        prompt: props.account?.platform === 'grok' || supportsImageTest.value ? testPrompt.value.trim() : '',
        mode: props.account?.platform === 'grok' ? grokTestMode.value : (isOpenAIAccount.value ? testMode.value : 'default')
      }),
      signal: controller.signal
    })

    if (!current()) { await response.body?.cancel(); return }
    if (!response.ok) {
      const detail = await response.json().catch(() => null)
      throw new Error(extractApiErrorMessage({ response: { data: detail } }, `HTTP ${response.status}`))
    }

    const reader = response.body?.getReader()
    if (!reader) {
      throw new Error('No response body')
    }

    const decoder = new TextDecoder()
    let buffer = ''
    let receivedBytes = 0

    try {
    readStream: while (true) {
      const { done, value } = await reader.read()
      if (!current()) { await reader.cancel(); return }
      if (done) { buffer += decoder.decode(); break }

      receivedBytes += value.byteLength
      if (receivedBytes > 32 * 1024 * 1024) throw new Error(t('admin.accounts.testStreamTooLarge'))
      buffer += decoder.decode(value, { stream: true })
      if (buffer.length > 32 * 1024 * 1024) throw new Error(t('admin.accounts.testStreamTooLarge'))
      const lines = buffer.split('\n')
      buffer = lines.pop() || ''

      for (const line of lines) {
        if (line.startsWith('data:')) {
          const jsonStr = line.slice(5).trim()
          if (jsonStr) {
            try {
              const event = JSON.parse(jsonStr)
              handleEvent(event)
              if (status.value !== 'connecting') break readStream
            } catch (e) {
              console.error('Failed to parse SSE event:', e)
            }
          }
        }
      }
    }
    if (status.value === 'connecting' && buffer.trim().startsWith('data:')) {
      try { handleEvent(JSON.parse(buffer.trim().slice(5).trim())) } catch { /* Incomplete final frame is handled below. */ }
    }
    if (status.value === 'connecting') {
      throw new Error(t('admin.accounts.testStreamIncomplete'))
    }
    } finally {
      await reader.cancel().catch(() => undefined)
      reader.releaseLock()
    }
  } catch (error: unknown) {
    if (!current()) return
    if (error instanceof DOMException && error.name === 'AbortError') {
      status.value = 'idle'
      return
    }
    status.value = 'error'
    const msg = error instanceof Error ? error.message : 'Unknown error'
    errorMessage.value = msg
    addLine(`Error: ${msg}`, 'text-red-400')
  } finally {
    if (current()) abortController = null
  }
}

const handleEvent = (event: {
  type: string
  text?: string
  model?: string
  success?: boolean
  error?: string
  code?: string
  image_url?: string
  video_url?: string
  audio_url?: string
  mime_type?: string
}) => {
  switch (event.type) {
    case 'test_start':
      addLine(t('admin.accounts.connectedToApi'), 'text-green-400')
      if (event.model) {
        addLine(t('admin.accounts.usingModel', { model: event.model }), 'text-cyan-400')
      }
      addLine(
        supportsImageTest.value
            ? t('admin.accounts.sendingImageRequest')
            : t('admin.accounts.sendingTestMessage'),
        'text-gray-400'
      )
      addLine('', 'text-gray-300')
      addLine(t('admin.accounts.response'), 'text-yellow-400')
      break

    case 'content':
      if (event.text) {
        streamingContent.value += event.text
        scrollToBottom()
      }
      break

    case 'status':
      if (event.text || event.code) {
        addLine(event.text || (event.code === 'tts_success' ? t('admin.accounts.ttsSuccess') : event.code || ''), 'text-cyan-300')
      }
      break

    case 'image':
      if (event.image_url) {
        generatedImages.value.push({
          url: event.image_url,
          mimeType: event.mime_type
        })
        addLine(t('admin.accounts.imageReceived', { count: generatedImages.value.length }), 'text-purple-300')
      }
      break

    case 'video':
      if (event.video_url || event.image_url) generatedVideos.value.push(event.video_url || event.image_url || '')
      break

    case 'audio':
      if (event.audio_url) generatedAudio.value.push(event.audio_url)
      break

    case 'test_complete':
      // Move streaming content to output lines
      if (streamingContent.value) {
        addLine(streamingContent.value, 'text-green-300')
        streamingContent.value = ''
      }
      if (event.success) {
        status.value = 'success'
      } else {
        status.value = 'error'
        errorMessage.value = event.error || 'Test failed'
      }
      break

    case 'error':
      status.value = 'error'
      errorMessage.value = event.error || 'Unknown error'
      if (streamingContent.value) {
        addLine(streamingContent.value, 'text-green-300')
        streamingContent.value = ''
      }
      break
  }
}

const copyOutput = () => {
  const text = outputLines.value.map((l) => l.text).join('\n')
  copyToClipboard(text, t('admin.accounts.outputCopied'))
}

watch(
  () => [props.show, props.account?.id] as const,
  ([show]) => {
    modelGeneration++
    abortStream()
    availableModels.value = []
    selectedModelId.value = ''
    loadingModels.value = false
    modelLoadError.value = ''
    testPrompt.value = ''
    clearGrokMedia()
    testMode.value = 'default'
    grokTestMode.value = 'text'
    resetState()
    if (show && props.account) void loadAvailableModels()
  },
  { immediate: true }
)
onBeforeUnmount(() => {
  disposed = true
  modelGeneration++
  abortStream()
})

</script>

<style>
.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.2s ease;
}
.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}
</style>
