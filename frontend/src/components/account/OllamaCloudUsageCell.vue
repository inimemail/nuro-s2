<template>
  <div v-if="eligible" class="mt-1 w-36 space-y-1.5 text-[10px]">
    <div class="flex items-center justify-between gap-2 text-gray-500 dark:text-gray-400">
      <span class="flex items-center gap-1 font-medium"><Icon name="cloud" size="xs" /> Ollama</span>
      <button v-if="state?.configured" type="button" class="rounded p-0.5 text-primary-600 hover:bg-primary-50 disabled:opacity-50 dark:text-primary-400 dark:hover:bg-dark-700" :disabled="loading" :title="t('admin.accounts.ollamaRefreshNow')" @click="refresh">
        <Icon name="refresh" size="xs" :class="{ 'animate-spin': loading }" />
      </button>
    </div>
    <UsageBar :label="t('admin.accounts.ollamaFiveHourShort')" :value="state?.snapshot?.data?.five_hour?.used_percent" />
    <UsageBar :label="t('admin.accounts.ollamaSevenDayShort')" :value="state?.snapshot?.data?.seven_day?.used_percent" />
  </div>
</template>

<script setup lang="ts">
import { computed, defineComponent, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import Icon from '@/components/icons/Icon.vue'
import type { Account, OllamaCloudUsageState } from '@/types'

const props = defineProps<{ account: Account }>()
const { t } = useI18n()
const state = ref<OllamaCloudUsageState | null>(null)
const loading = ref(false)
const eligible = computed(() => {
  const baseURL = String((props.account.credentials as Record<string, unknown> | undefined)?.base_url || '').replace(/\/$/, '').toLowerCase()
  return props.account.type === 'apikey' && ['openai', 'anthropic', 'kimi', 'zhipu', 'deepseek'].includes(props.account.platform) && ['https://ollama.com', 'https://ollama.com/v1'].includes(baseURL)
})
const UsageBar = defineComponent({
  props: { label: { type: String, required: true }, value: { type: Number, default: undefined } },
  setup(componentProps) { return () => { const value = Math.max(0, Math.min(100, componentProps.value ?? 0)); return h('div', { class: 'flex items-center gap-1.5' }, [h('span', { class: 'w-4 shrink-0 text-gray-400' }, componentProps.label), h('div', { class: 'h-1 flex-1 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-700' }, [h('div', { class: value >= 90 ? 'h-full bg-red-500' : value >= 70 ? 'h-full bg-amber-500' : 'h-full bg-emerald-500', style: { width: `${value}%` } })]), h('span', { class: 'w-7 text-right tabular-nums text-gray-500 dark:text-gray-400' }, componentProps.value == null ? '--' : `${Math.round(value)}%`)]) } }
})
async function load() { if (!eligible.value) return; loading.value = true; try { state.value = await adminAPI.accounts.getOllamaCloudUsage(props.account.id) } finally { loading.value = false } }
async function refresh() { loading.value = true; try { state.value = await adminAPI.accounts.refreshOllamaCloudUsage(props.account.id) } finally { loading.value = false } }
onMounted(() => { void load() })
</script>
