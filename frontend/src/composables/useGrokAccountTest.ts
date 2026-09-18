import { computed, ref, watch, type Ref } from 'vue'
import type { ClaudeModel } from '@/types'

export type AccountTestModel = ClaudeModel & { upstream_model?: string }

function modelKind(model: AccountTestModel) {
  const id = (model.upstream_model || model.id).toLowerCase().replace(/^(xai|x-ai|grok)\//, '')
  if (id.startsWith('grok-imagine-video') || id.startsWith('grok-video')) return 'video'
  if (id === 'grok-imagine' || id === 'grok-imagine-edit' || id.startsWith('grok-imagine-image')) return 'image'
  return 'text'
}

export function useGrokAccountTest(
  isGrok: Ref<boolean>,
  models: Ref<AccountTestModel[]>,
  selected: Ref<string>,
  prompt: Ref<string>,
  reportError: (key: string) => void
) {
  const mode = ref('text')
  const media = ref('')
  const mediaName = ref('')
  const readingMedia = ref(false)
  let uploadVersion = 0
  const needsModel = computed(() => !isGrok.value || ['text', 'chat', 'image', 'video', 'search'].includes(mode.value))
  const options = computed(() => {
    const kind = ['image', 'video'].includes(mode.value) ? mode.value : 'text'
    return models.value.filter(model => !isGrok.value || modelKind(model) === kind).map(model => ({ ...model }))
  })
  const supportsPrompt = computed(() => isGrok.value && !['stt', 'realtime'].includes(mode.value))
  const canStart = computed(() => !readingMedia.value && (!needsModel.value || !!selected.value) && (!isGrok.value || mode.value !== 'stt' || !!media.value))

  function clearMedia() {
    uploadVersion++
    media.value = ''
    mediaName.value = ''
    readingMedia.value = false
  }

  watch([options, needsModel], () => {
    if (isGrok.value && needsModel.value && !options.value.some(model => model.id === selected.value)) {
      selected.value = options.value[0]?.id || ''
    }
  })
  watch(mode, () => {
    clearMedia()
    if (prompt.value.startsWith('data:')) prompt.value = ''
  })

  function upload(event: Event) {
    const input = event.target as HTMLInputElement
    const file = input.files?.[0]
    clearMedia()
    if (!file) return
    const prefix = mode.value === 'stt' ? 'audio/' : 'image/'
    if (!file.type.startsWith(prefix)) {
      reportError('admin.accounts.grokMediaWrongType')
      input.value = ''
      return
    }
    if (file.size > 8 * 1024 * 1024) {
      reportError('admin.accounts.grokMediaTooLarge')
      input.value = ''
      return
    }
    const version = uploadVersion
    const reader = new FileReader()
    readingMedia.value = true
    reader.onload = () => {
      if (version !== uploadVersion) return
      readingMedia.value = false
      if (typeof reader.result === 'string') {
        media.value = reader.result
        mediaName.value = file.name
      }
    }
    reader.onerror = () => {
      if (version !== uploadVersion) return
      clearMedia()
      reportError('admin.accounts.grokMediaReadFailed')
    }
    reader.readAsDataURL(file)
  }

  const requestFields = computed(() => {
    if (!isGrok.value || !media.value) return {}
    return mode.value === 'stt' ? { audio_data_url: media.value } : { image_data_url: media.value }
  })
  const requestModel = computed(() => needsModel.value ? selected.value : mode.value === 'realtime' ? 'grok-voice-latest' : '')
  return { mode, options, needsModel, supportsPrompt, canStart, mediaName, clearMedia, upload, requestFields, requestModel }
}
