<template>
  <div v-if="eligible" class="mt-1 space-y-1 text-[10px]">
    <div class="flex items-center gap-1 text-gray-500 dark:text-gray-400">
      <span class="font-medium">Ollama</span>
      <span v-if="loading" class="animate-pulse">...</span>
      <span v-else-if="state?.snapshot" :class="state.snapshot.status === 'ok' ? 'text-emerald-600' : 'text-amber-600'">
        {{ state.snapshot.data?.five_hour ? `${Math.round(state.snapshot.data.five_hour.used_percent)}%` : state.snapshot.status }}
      </span>
      <button v-if="state?.configured" type="button" class="text-blue-600 hover:underline" :disabled="loading" @click="refresh">{{ loading ? '...' : 'Refresh' }}</button>
    </div>
    <div v-if="state?.snapshot?.data?.seven_day" class="text-gray-400">7d {{ Math.round(state.snapshot.data.seven_day.used_percent) }}%</div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { adminAPI } from '@/api/admin'
import type { Account, OllamaCloudUsageState } from '@/types'

const props = defineProps<{ account: Account }>()
const state = ref<OllamaCloudUsageState | null>(null)
const loading = ref(false)
const eligible = computed(() => props.account.type === 'apikey' && (props.account.platform === 'openai' || props.account.platform === 'anthropic') && ['https://ollama.com', 'https://ollama.com/v1'].includes(String((props.account.credentials as any)?.base_url || '').replace(/\/$/, '')))
const load = async () => { if (!eligible.value) return; loading.value = true; try { state.value = await adminAPI.accounts.getOllamaCloudUsage(props.account.id) } finally { loading.value = false } }
const refresh = async () => { loading.value = true; try { state.value = await adminAPI.accounts.refreshOllamaCloudUsage(props.account.id) } finally { loading.value = false } }
onMounted(load)
</script>
