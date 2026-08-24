<template>
  <BaseDialog
    :show="show"
    :title="editing ? t('admin.channelMonitor.editTitle') : t('admin.channelMonitor.createTitle')"
    width="wide"
    @close="$emit('close')"
  >
    <form id="channel-monitor-form" @submit.prevent="handleSubmit" class="space-y-5">
      <div>
        <label class="input-label">{{ t('admin.channelMonitor.form.name') }} <span class="text-red-500">*</span></label>
        <input v-model="form.name" type="text" required class="input" :placeholder="t('admin.channelMonitor.form.namePlaceholder')" />
      </div>

      <div>
        <label class="input-label">{{ t('admin.channelMonitor.form.checkMode') }}</label>
        <div class="grid gap-2 sm:grid-cols-3">
          <button
            v-for="opt in checkModeOptions"
            :key="opt.value"
            type="button"
            :aria-pressed="form.check_mode === opt.value"
            :disabled="opt.disabled"
            class="min-h-[68px] rounded-lg border-2 px-3 py-2 text-left transition-colors disabled:cursor-not-allowed disabled:opacity-45"
            :class="checkModeButtonClass(opt.value)"
            @click="selectCheckMode(opt.value)"
          >
            <span class="block text-sm font-semibold">{{ opt.label }}</span>
            <span class="mt-1 block text-xs leading-4 opacity-75">{{ opt.hint }}</span>
          </button>
        </div>
      </div>

      <div>
        <label class="input-label">{{ t('admin.channelMonitor.form.provider') }} <span class="text-red-500">*</span></label>
        <div class="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <button
            v-for="opt in providerOptions"
            :key="opt.value"
            type="button"
            :aria-pressed="form.provider === opt.value"
            class="flex items-center justify-center gap-2 rounded-lg border-2 px-3 py-2.5 text-sm font-medium transition-colors"
            :class="providerPickerClass(opt.value, form.provider === opt.value)"
            @click="selectProvider(opt.value)"
          >
            <ProviderIcon :provider="opt.value" :size="18" />
            <span>{{ opt.label }}</span>
          </button>
        </div>
      </div>

      <div v-if="usesQuotaMode" class="rounded-lg border border-blue-100 bg-blue-50/50 p-3 dark:border-blue-500/20 dark:bg-blue-500/10">
        <label class="input-label">
          {{ t('admin.channelMonitor.form.linkedAccount') }} <span class="text-red-500">*</span>
        </label>
        <Select
          v-model="accountSelectValue"
          :options="accountOptions"
          :placeholder="t('admin.channelMonitor.form.linkedAccountPlaceholder')"
          remote
          :loading="accountsLoading"
          @search="onAccountSearch"
        />
        <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.channelMonitor.form.linkedAccountHint') }}</p>
        <p v-if="accountHydrationFailed" class="mt-1 text-xs text-amber-600 dark:text-amber-400">{{ t('admin.channelMonitor.form.linkedAccountMissing') }}</p>
        <p v-else-if="accountOptions.length === 0 && !accountsLoading" class="mt-1 text-xs text-amber-600 dark:text-amber-400">{{ t('admin.channelMonitor.form.linkedAccountEmpty') }}</p>
      </div>

      <div v-if="form.provider === PROVIDER_OPENAI && usesProbePart" class="rounded-lg border border-blue-100 bg-blue-50/50 p-3 dark:border-blue-500/20 dark:bg-blue-500/10">
        <label class="input-label">{{ t('admin.channelMonitor.form.apiMode') }}</label>
        <div class="grid gap-3 sm:grid-cols-2">
          <button
            v-for="opt in apiModeOptions"
            :key="opt.value"
            type="button"
            :aria-pressed="form.api_mode === opt.value"
            class="rounded-lg border-2 px-3 py-2 text-left transition-colors"
            :class="apiModeButtonClass(opt.value)"
            @click="form.api_mode = opt.value"
          >
            <span class="block text-sm font-semibold">{{ opt.label }}</span>
            <span class="mt-0.5 block text-xs opacity-80">{{ opt.hint }}</span>
          </button>
        </div>
      </div>

      <div v-if="usesProbePart">
        <label class="input-label">{{ t('admin.channelMonitor.form.endpoint') }} <span class="text-red-500">*</span></label>
        <div class="flex flex-col gap-2 sm:flex-row">
          <input v-model="form.endpoint" type="text" required class="input flex-1" :placeholder="t('admin.channelMonitor.form.endpointPlaceholder')" />
          <button type="button" @click="useCurrentDomain" class="btn btn-secondary w-full whitespace-nowrap sm:w-auto">
            {{ t('admin.channelMonitor.form.useCurrentDomain') }}
          </button>
        </div>
      </div>

      <div v-if="usesProbePart">
        <label class="input-label">
          {{ t('admin.channelMonitor.form.apiKey') }}<span v-if="!editing" class="text-red-500"> *</span>
        </label>
        <div class="flex flex-col gap-2 sm:flex-row">
          <input
            v-model="form.api_key"
            type="password"
            :required="!editing"
            class="input flex-1"
            :placeholder="editing ? t('admin.channelMonitor.form.apiKeyEditPlaceholder') : t('admin.channelMonitor.form.apiKeyPlaceholder')"
          />
          <button type="button" @click="openMyKeyPicker" class="btn btn-secondary w-full whitespace-nowrap sm:w-auto">
            {{ t('admin.channelMonitor.form.useMyKey') }}
          </button>
        </div>
        <p v-if="editing && editing.api_key_masked" class="mt-1 text-xs text-gray-400">{{ editing.api_key_masked }}</p>
      </div>

      <div v-if="usesProbePart">
        <label class="input-label">{{ t('admin.channelMonitor.form.primaryModel') }} <span class="text-red-500">*</span></label>
        <input
          v-model="form.primary_model"
          type="text"
          required
          class="input font-medium"
          :class="getPlatformTextClass(form.provider)"
          :placeholder="t('admin.channelMonitor.form.primaryModelPlaceholder')"
        />
      </div>

      <div v-if="usesProbePart">
        <label class="input-label">{{ t('admin.channelMonitor.form.extraModels') }}</label>
        <ModelTagInput
          :models="form.extra_models"
          :platform="form.provider"
          :placeholder="t('admin.channelMonitor.form.extraModelsPlaceholder')"
          @update:models="form.extra_models = $event"
        />
      </div>

      <div>
        <label class="input-label">{{ t('admin.channelMonitor.form.groupName') }}</label>
        <input v-model="form.group_name" type="text" class="input" :placeholder="t('admin.channelMonitor.form.groupNamePlaceholder')" />
      </div>

      <div>
        <label class="input-label">{{ t('admin.channelMonitor.form.intervalSeconds') }} <span class="text-red-500">*</span></label>
        <input v-model.number="form.interval_seconds" type="number" min="15" max="3600" required class="input" />
        <p class="mt-1 text-xs text-gray-400">{{ t('admin.channelMonitor.form.intervalSecondsHint') }}</p>
      </div>

      <div class="flex items-center justify-between">
        <label class="input-label mb-0">{{ t('admin.channelMonitor.form.enabled') }}</label>
        <Toggle v-model="form.enabled" />
      </div>

      <!-- 高级设置区：请求模板 + 自定义 headers/body -->
      <details v-if="usesProbePart" class="rounded-lg border border-gray-200 bg-gray-50/50 p-3 dark:border-dark-700 dark:bg-dark-900/30">
        <summary class="cursor-pointer text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.channelMonitor.advanced.section') }}
        </summary>
        <p class="mt-1 text-xs text-gray-400">{{ t('admin.channelMonitor.advanced.sectionHint') }}</p>

        <div class="mt-4 space-y-4">
          <div>
            <label class="input-label">{{ t('admin.channelMonitor.templateField.label') }}</label>
            <Select
              v-model="templateSelectValue"
              :options="templateOptions"
              :placeholder="t('admin.channelMonitor.templateField.placeholder')"
            />
            <p class="mt-1 text-xs text-gray-400">{{ t('admin.channelMonitor.templateField.applyHint') }}</p>
          </div>

          <MonitorAdvancedRequestConfig
            :provider="form.provider"
            :api-mode="form.api_mode"
            :extra-headers="form.extra_headers"
            :body-override-mode="form.body_override_mode"
            :body-override="form.body_override"
            @update:extra-headers="form.extra_headers = $event"
            @update:body-override-mode="form.body_override_mode = $event"
            @update:body-override="form.body_override = $event"
          />
        </div>
      </details>
    </form>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button @click="$emit('close')" type="button" class="btn btn-secondary">
          {{ t('common.cancel') }}
        </button>
        <button
          type="submit"
          form="channel-monitor-form"
          :disabled="submitting"
          class="btn btn-primary"
        >
          {{ submitting
            ? t('common.submitting')
            : editing ? t('common.update') : t('common.create') }}
        </button>
      </div>
    </template>
  </BaseDialog>

  <MonitorKeyPickerDialog
    :show="showKeyPicker"
    :loading="myKeysLoading"
    :keys="myActiveKeys"
    :provider="form.provider"
    :user-group-rates="userGroupRates"
    @close="showKeyPicker = false"
    @pick="pickMyKey"
  />
</template>

<script setup lang="ts">
import { ref, reactive, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { adminAPI } from '@/api/admin'
import { keysAPI } from '@/api/keys'
import { userGroupsAPI } from '@/api/groups'
import type {
  BodyOverrideMode,
  ChannelMonitor,
  CreateParams,
  APIMode,
  CheckMode,
  Provider,
  UpdateParams,
} from '@/api/admin/channelMonitor'
import type { ChannelMonitorTemplate } from '@/api/admin/channelMonitorTemplate'
import type { ApiKey } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Toggle from '@/components/common/Toggle.vue'
import Select from '@/components/common/Select.vue'
import ModelTagInput from '@/components/admin/channel/ModelTagInput.vue'
import { getPlatformTextClass } from '@/components/admin/channel/types'
import MonitorKeyPickerDialog from '@/components/admin/monitor/MonitorKeyPickerDialog.vue'
import MonitorAdvancedRequestConfig from '@/components/admin/monitor/MonitorAdvancedRequestConfig.vue'
import ProviderIcon from '@/components/user/monitor/ProviderIcon.vue'
import { useChannelMonitorFormat } from '@/composables/useChannelMonitorFormat'
import {
  PROVIDER_OPENAI,
  PROVIDER_ANTHROPIC,
  PROVIDER_GEMINI,
  PROVIDER_GROK,
  PROVIDER_ANTIGRAVITY,
  PROVIDER_KIMI,
  PROVIDER_ZHIPU,
  PROVIDER_DEEPSEEK,
  CHECK_MODE_PROBE,
  CHECK_MODE_QUOTA,
  CHECK_MODE_QUOTA_PROBE,
  API_MODE_CHAT_COMPLETIONS,
  API_MODE_RESPONSES,
  DEFAULT_GROK_ENDPOINT,
  DEFAULT_GROK_MODEL,
  DEFAULT_KIMI_ENDPOINT,
  DEFAULT_ZHIPU_ENDPOINT,
  DEFAULT_DEEPSEEK_ENDPOINT,
  DEFAULT_INTERVAL_SECONDS,
} from '@/constants/channelMonitor'

const props = defineProps<{
  show: boolean
  monitor: ChannelMonitor | null
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'saved'): void
}>()

const { t } = useI18n()
const appStore = useAppStore()
const { providerPickerClass } = useChannelMonitorFormat()

// System-configured default interval for new monitors. Falls back to the static
// constant when public settings haven't loaded yet or store the legacy 0 value.
const systemDefaultInterval = computed<number>(() => {
  const configured = appStore.cachedPublicSettings?.channel_monitor_default_interval_seconds
  return configured && configured > 0 ? configured : DEFAULT_INTERVAL_SECONDS
})

// editing is true when we have an existing monitor
const editing = computed<ChannelMonitor | null>(() => props.monitor)

const submitting = ref(false)

// API key picker
const showKeyPicker = ref(false)
const myKeysLoading = ref(false)
const myActiveKeys = ref<ApiKey[]>([])
const userGroupRates = ref<Record<number, number>>({})

interface MonitorForm {
  name: string
  provider: Provider
  api_mode: APIMode
  check_mode: CheckMode
  account_id: number | null
  endpoint: string
  api_key: string
  primary_model: string
  extra_models: string[]
  group_name: string
  interval_seconds: number
  enabled: boolean
  // 高级设置快照
  template_id: number | null
  extra_headers: Record<string, string>
  body_override_mode: BodyOverrideMode
  body_override: Record<string, unknown> | null
}

const form = reactive<MonitorForm>({
  name: '',
  provider: PROVIDER_ANTHROPIC,
  api_mode: API_MODE_CHAT_COMPLETIONS,
  check_mode: CHECK_MODE_PROBE,
  account_id: null,
  endpoint: '',
  api_key: '',
  primary_model: '',
  extra_models: [],
  group_name: '',
  interval_seconds: systemDefaultInterval.value,
  enabled: true,
  template_id: null,
  extra_headers: {},
  body_override_mode: 'off',
  body_override: null,
})

const usesQuotaMode = computed(() => form.check_mode !== CHECK_MODE_PROBE)
const usesProbePart = computed(() => form.check_mode !== CHECK_MODE_QUOTA)

let suppressFormWatchers = false

// 可用模板列表（进入 dialog 时一次性拉取 cache；按 provider / api mode 过滤）。
const templatesCache = ref<ChannelMonitorTemplate[]>([])
const templatesLoading = ref(false)

const templateOptions = computed(() => {
  const items = templatesCache.value.filter((t) => {
    if (t.provider !== form.provider) return false
    if (form.provider !== PROVIDER_OPENAI) return true
    return normalizeAPIMode(t.api_mode) === form.api_mode
  })
  return [
    { value: '', label: t('admin.channelMonitor.templateField.none') },
    ...items.map((t) => ({ value: String(t.id), label: templateOptionLabel(t) })),
  ]
})

async function loadTemplates() {
  if (templatesCache.value.length > 0) return
  templatesLoading.value = true
  try {
    const { items } = await adminAPI.channelMonitorTemplate.list()
    templatesCache.value = items
  } catch (err: unknown) {
    // 模板拉取失败不阻塞监控表单，用户可以不选模板
    console.warn('load monitor templates failed', err)
  } finally {
    templatesLoading.value = false
  }
}

// 模板下拉绑定：value 是 string（Select 组件约束），需要与 number | null 互转。
const templateSelectValue = computed<string>({
  get: () => (form.template_id == null ? '' : String(form.template_id)),
  set: (raw: string) => {
    if (raw === '') {
      form.template_id = null
      return
    }
    const id = Number(raw)
    if (!Number.isFinite(id)) return
    form.template_id = id
    // 应用模板 = 拷贝快照
    const tpl = templatesCache.value.find((t) => t.id === id)
    if (tpl) {
      suppressFormWatchers = true
      form.api_mode = normalizeAPIMode(tpl.api_mode)
      form.template_id = id
      form.extra_headers = { ...(tpl.extra_headers || {}) }
      form.body_override_mode = tpl.body_override_mode
      form.body_override = tpl.body_override ? { ...tpl.body_override } : null
      suppressFormWatchers = false
    }
  },
})

const apiModeOptions = computed<{ value: APIMode; label: string; hint: string }[]>(() => [
  {
    value: API_MODE_CHAT_COMPLETIONS,
    label: t('admin.channelMonitor.form.apiModeChatCompletions'),
    hint: t('admin.channelMonitor.form.apiModeChatCompletionsHint'),
  },
  {
    value: API_MODE_RESPONSES,
    label: t('admin.channelMonitor.form.apiModeResponses'),
    hint: t('admin.channelMonitor.form.apiModeResponsesHint'),
  },
])

function normalizeAPIMode(mode: APIMode | undefined | null): APIMode {
  return mode === API_MODE_RESPONSES ? API_MODE_RESPONSES : API_MODE_CHAT_COMPLETIONS
}

function apiModeButtonClass(mode: APIMode): string {
  const active = form.api_mode === mode
  if (active) {
    return 'border-primary-500 bg-white text-primary-700 shadow-sm dark:border-primary-400 dark:bg-primary-500/15 dark:text-primary-300'
  }
  return 'border-blue-100 bg-white/70 text-gray-600 hover:border-primary-300 dark:border-dark-700 dark:bg-dark-800 dark:text-gray-400'
}

function templateOptionLabel(tpl: ChannelMonitorTemplate): string {
  if (tpl.provider !== PROVIDER_OPENAI) return tpl.name
  const labelKey = normalizeAPIMode(tpl.api_mode) === API_MODE_RESPONSES
    ? 'admin.channelMonitor.form.apiModeResponses'
    : 'admin.channelMonitor.form.apiModeChatCompletions'
  return `${tpl.name} · ${t(labelKey)}`
}

function clearRequestSnapshot() {
  form.template_id = null
  form.extra_headers = {}
  form.body_override_mode = 'off'
  form.body_override = null
}

interface ProviderOption {
  value: Provider
  label: string
}

const providerOptions = computed<ProviderOption[]>(() => [
  { value: PROVIDER_ANTHROPIC, label: t('monitorCommon.providers.anthropic') },
  { value: PROVIDER_OPENAI, label: t('monitorCommon.providers.openai') },
  { value: PROVIDER_GEMINI, label: t('monitorCommon.providers.gemini') },
  { value: PROVIDER_GROK, label: t('monitorCommon.providers.grok') },
  { value: PROVIDER_ANTIGRAVITY, label: t('monitorCommon.providers.antigravity') },
  { value: PROVIDER_KIMI, label: t('monitorCommon.providers.kimi') },
  { value: PROVIDER_ZHIPU, label: t('monitorCommon.providers.zhipu') },
  { value: PROVIDER_DEEPSEEK, label: t('monitorCommon.providers.deepseek') },
])

const checkModeOptions = computed(() => [
  { value: CHECK_MODE_PROBE, label: t('admin.channelMonitor.form.checkModeProbe'), hint: t('admin.channelMonitor.form.checkModeProbeHint'), disabled: form.provider === PROVIDER_ANTIGRAVITY },
  { value: CHECK_MODE_QUOTA, label: t('admin.channelMonitor.form.checkModeQuota'), hint: t('admin.channelMonitor.form.checkModeQuotaHint'), disabled: false },
  { value: CHECK_MODE_QUOTA_PROBE, label: t('admin.channelMonitor.form.checkModeQuotaProbe'), hint: t('admin.channelMonitor.form.checkModeQuotaProbeHint'), disabled: form.provider === PROVIDER_ANTIGRAVITY },
])

function checkModeButtonClass(mode: CheckMode): string {
  return form.check_mode === mode
    ? 'border-blue-500 bg-blue-50 text-blue-800 shadow-sm dark:border-blue-400 dark:bg-blue-500/15 dark:text-blue-200'
    : 'border-gray-200 bg-white text-gray-600 hover:border-blue-300 dark:border-dark-700 dark:bg-dark-800 dark:text-gray-400'
}

function selectCheckMode(mode: CheckMode) {
  if (checkModeOptions.value.find((option) => option.value === mode)?.disabled) return
  form.check_mode = mode
  if (!usesQuotaMode.value) form.account_id = null
}

interface LinkedAccount { id: number; name: string; type: string }
const linkedAccounts = ref<LinkedAccount[]>([])
const accountsLoading = ref(false)
const accountHydrationFailed = ref(false)
const pinnedAccount = ref<LinkedAccount | null>(null)
let accountRequest = 0

const accountOptions = computed(() => {
  const accounts = [...linkedAccounts.value]
  if (pinnedAccount.value && !accounts.some((account) => account.id === pinnedAccount.value?.id)) accounts.unshift(pinnedAccount.value)
  return accounts.map((account) => ({ value: String(account.id), label: `${account.name} · ${account.type} · #${account.id}` }))
})

const accountSelectValue = computed<string>({
  get: () => form.account_id == null ? '' : String(form.account_id),
  set: (raw) => {
    const id = Number(raw)
    form.account_id = Number.isSafeInteger(id) && id > 0 ? id : null
  },
})

async function loadLinkedAccounts(search = '') {
  if (!props.show || !usesQuotaMode.value) return
  const request = ++accountRequest
  accountsLoading.value = true
  try {
    const response = await adminAPI.accounts.list(1, 50, { platform: form.provider, ...(search ? { search } : {}) })
    if (request !== accountRequest) return
    linkedAccounts.value = (response.items || []).map((account) => ({ id: account.id, name: account.name, type: account.type }))
  } catch (error) {
    if (request === accountRequest) linkedAccounts.value = []
    console.warn('load linked monitor accounts failed', error)
  } finally {
    if (request === accountRequest) accountsLoading.value = false
  }
}

async function hydrateSelectedAccount() {
  if (!form.account_id || !usesQuotaMode.value) return
  try {
    const account = await adminAPI.accounts.getById(form.account_id)
    if (String(account.platform) !== form.provider) throw new Error('platform mismatch')
    pinnedAccount.value = { id: account.id, name: account.name, type: account.type }
    accountHydrationFailed.value = false
  } catch {
    accountHydrationFailed.value = true
  }
}

function onAccountSearch(query: string) { void loadLinkedAccounts(query) }

watch(() => [props.show, form.provider, form.check_mode] as const, ([show]) => {
  if (show && usesQuotaMode.value) void loadLinkedAccounts()
}, { immediate: true })

function selectProvider(provider: Provider) {
  if (form.provider === provider) return
  const previousProvider = form.provider
  const clearGrokEndpoint = previousProvider === PROVIDER_GROK && form.endpoint === DEFAULT_GROK_ENDPOINT
  const clearGrokModel = previousProvider === PROVIDER_GROK && form.primary_model === DEFAULT_GROK_MODEL
	const defaults: Partial<Record<Provider, string>> = {
		[PROVIDER_KIMI]: DEFAULT_KIMI_ENDPOINT,
		[PROVIDER_ZHIPU]: DEFAULT_ZHIPU_ENDPOINT,
		[PROVIDER_DEEPSEEK]: DEFAULT_DEEPSEEK_ENDPOINT,
	}
  form.provider = provider
  form.account_id = null
  pinnedAccount.value = null
  accountHydrationFailed.value = false
  if (provider === PROVIDER_ANTIGRAVITY) form.check_mode = CHECK_MODE_QUOTA
  if (previousProvider === PROVIDER_ANTIGRAVITY && provider !== PROVIDER_ANTIGRAVITY) form.check_mode = CHECK_MODE_PROBE
  if (provider === PROVIDER_GROK) {
    if (!form.endpoint.trim()) form.endpoint = DEFAULT_GROK_ENDPOINT
    if (!form.primary_model.trim()) form.primary_model = DEFAULT_GROK_MODEL
    return
  }
  if (clearGrokEndpoint) form.endpoint = ''
  if (clearGrokModel) form.primary_model = ''
	if (defaults[provider] && !form.endpoint.trim()) form.endpoint = defaults[provider] || ''
}

// Clear api_key whenever provider changes to avoid cross-provider key mismatch.
// Editing mode loads api_key='' via loadFromMonitor and only sets it on user
// typing, so clearing on provider change is always a safe no-op until the user
// picks a new key.
// 同时清空 template_id（模板有 provider 归属，跨平台不通用）。
watch(() => form.provider, () => {
  if (suppressFormWatchers) return
  form.api_key = ''
  if (form.provider !== PROVIDER_OPENAI) {
    form.api_mode = API_MODE_CHAT_COMPLETIONS
  }
  clearRequestSnapshot()
}, { flush: 'sync' })

watch(() => form.api_mode, () => {
  if (suppressFormWatchers) return
  if (form.provider === PROVIDER_OPENAI) {
    clearRequestSnapshot()
  }
}, { flush: 'sync' })

function resetForm() {
  suppressFormWatchers = true
  form.name = ''
  form.provider = PROVIDER_ANTHROPIC
  form.api_mode = API_MODE_CHAT_COMPLETIONS
  form.check_mode = CHECK_MODE_PROBE
  form.account_id = null
  form.endpoint = ''
  form.api_key = ''
  form.primary_model = ''
  form.extra_models = []
  form.group_name = ''
  form.interval_seconds = systemDefaultInterval.value
  form.enabled = true
  form.template_id = null
  form.extra_headers = {}
  form.body_override_mode = 'off'
  form.body_override = null
  suppressFormWatchers = false
}

function loadFromMonitor(m: ChannelMonitor) {
  suppressFormWatchers = true
  form.name = m.name
  form.provider = m.provider
  form.api_mode = normalizeAPIMode(m.api_mode)
  form.check_mode = m.check_mode || CHECK_MODE_PROBE
  form.account_id = m.account_id ?? null
  form.endpoint = m.endpoint
  form.api_key = ''
  form.primary_model = m.primary_model
  form.extra_models = [...(m.extra_models || [])]
  form.group_name = m.group_name || ''
  form.interval_seconds = m.interval_seconds || systemDefaultInterval.value
  form.enabled = m.enabled
  form.template_id = m.template_id ?? null
  form.extra_headers = { ...(m.extra_headers || {}) }
  form.body_override_mode = m.body_override_mode || 'off'
  form.body_override = m.body_override ? { ...m.body_override } : null
  suppressFormWatchers = false
	void hydrateSelectedAccount()
}

// Re-sync form whenever the dialog is opened or the target monitor changes.
// 同时拉取模板列表（cache 过的话一次性返回）。
watch(
  () => [props.show, props.monitor] as const,
  ([show, m]) => {
    if (!show) return
    void loadTemplates()
    if (m) loadFromMonitor(m)
    else resetForm()
  },
  { immediate: true },
)

function useCurrentDomain() {
  form.endpoint = window.location.origin
}

async function openMyKeyPicker() {
  showKeyPicker.value = true
  if (myActiveKeys.value.length > 0) return
  myKeysLoading.value = true
  try {
    const [res, rates] = await Promise.all([
      keysAPI.list(1, 100, { status: 'active' }),
      userGroupsAPI.getUserGroupRates(),
    ])
    const items = res.items || []
    const now = Date.now()
    myActiveKeys.value = items.filter(k => {
      if (k.status !== 'active') return false
      if (!k.expires_at) return true
      return new Date(k.expires_at).getTime() > now
    })
    userGroupRates.value = rates
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.channelMonitor.form.noActiveKey')))
  } finally {
    myKeysLoading.value = false
  }
}

function pickMyKey(k: ApiKey) {
  form.api_key = k.key
  showKeyPicker.value = false
}

function buildPayload(): CreateParams {
  return {
    name: form.name.trim(),
    provider: form.provider,
    api_mode: form.provider === PROVIDER_OPENAI ? form.api_mode : API_MODE_CHAT_COMPLETIONS,
    check_mode: form.check_mode,
    account_id: usesQuotaMode.value ? form.account_id : null,
    endpoint: usesProbePart.value ? form.endpoint.trim() : '',
    api_key: usesProbePart.value ? form.api_key.trim() : '',
    primary_model: usesProbePart.value ? form.primary_model.trim() : 'quota',
    extra_models: usesProbePart.value ? form.extra_models : [],
    group_name: form.group_name.trim(),
    enabled: form.enabled,
    interval_seconds: form.interval_seconds,
    template_id: form.template_id,
    extra_headers: form.extra_headers,
    body_override_mode: form.body_override_mode,
    body_override: form.body_override,
  }
}

async function handleSubmit() {
  if (submitting.value) return
  if (!form.name.trim()) {
    appStore.showError(t('admin.channelMonitor.nameRequired'))
    return
  }
  if (usesQuotaMode.value && !form.account_id) {
	appStore.showError(t('admin.channelMonitor.linkedAccountRequired'))
	return
	}
  if (usesProbePart.value && !form.primary_model.trim()) {
    appStore.showError(t('admin.channelMonitor.primaryModelRequired'))
    return
  }

  submitting.value = true
  try {
    const target = editing.value
    if (target) {
      const { api_key, ...rest } = buildPayload()
      const req: UpdateParams = { ...rest }
      // Only send api_key if user typed a new value
      if (api_key) req.api_key = api_key
      // template_id=null 用 clear_template=true 明确告诉后端清空（pointer 语义）
	  if (!usesQuotaMode.value) req.account_id = 0
      if (usesProbePart.value && form.template_id == null) {
        req.clear_template = true
        delete req.template_id
      }
      await adminAPI.channelMonitor.update(target.id, req)
      appStore.showSuccess(t('admin.channelMonitor.updateSuccess'))
    } else {
      await adminAPI.channelMonitor.create(buildPayload())
      appStore.showSuccess(t('admin.channelMonitor.createSuccess'))
    }
    emit('saved')
    emit('close')
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    submitting.value = false
  }
}
</script>
