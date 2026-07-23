<template>
  <div v-if="eligible" class="border-t border-gray-200 pt-4 dark:border-dark-700">
    <div class="mb-2 flex items-center justify-between gap-3">
      <label class="input-label mb-0">Ollama Cloud usage</label>
      <span v-if="state" class="text-xs text-gray-500">{{ state.configured ? 'Configured' : 'Not configured' }}</span>
    </div>
    <div class="space-y-2">
      <input v-model="session" type="password" autocomplete="off" class="input" placeholder="Cookie: name=value" />
      <div class="flex flex-wrap items-center gap-2">
        <button type="button" class="btn btn-secondary" :disabled="busy || !session.trim()" @click="save">Save session</button>
        <button v-if="state?.configured" type="button" class="btn btn-secondary" :disabled="busy" @click="refresh">Refresh usage</button>
        <button v-if="state?.configured" type="button" class="btn btn-secondary text-red-600" :disabled="busy" @click="remove">Remove</button>
        <label v-if="state?.configured" class="flex items-center gap-2 text-sm text-gray-600 dark:text-gray-300">
          <input type="checkbox" :checked="state.auto_refresh_enabled" :disabled="busy" @change="toggle(($event.target as HTMLInputElement).checked)" />
          Auto refresh
        </label>
      </div>
      <div v-if="globalSettings" class="flex flex-wrap items-center gap-3 text-sm text-gray-600 dark:text-gray-300">
        <label class="flex items-center gap-2"><input type="checkbox" v-model="globalSettings.enabled" :disabled="busy" @change="saveGlobal" /> Background refresh</label>
        <label class="flex items-center gap-2">Interval <input v-model.number="globalSettings.interval_minutes" type="number" min="15" max="1440" class="input w-24" :disabled="busy" @change="saveGlobal" /> min</label>
      </div>
      <p v-if="state && !state.encryption_key_configured" class="text-xs text-amber-600">A fixed TOTP_ENCRYPTION_KEY is required.</p>
      <p v-if="error" class="text-xs text-red-600">{{ error }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { adminAPI } from '@/api/admin'
import type { Account, OllamaCloudUsageState, OllamaCloudUsageSettings as GlobalSettings } from '@/types'
const props = defineProps<{ account: Account }>()
const state = ref<OllamaCloudUsageState | null>(null); const globalSettings = ref<GlobalSettings | null>(null); const session = ref(''); const busy = ref(false); const error = ref('')
const eligible = computed(() => props.account.type === 'apikey' && (props.account.platform === 'openai' || props.account.platform === 'anthropic') && ['https://ollama.com','https://ollama.com/v1'].includes(String((props.account.credentials as any)?.base_url || '').replace(/\/$/, '')))
const run = async (fn: () => Promise<OllamaCloudUsageState>) => { busy.value=true; error.value=''; try { state.value=await fn() } catch (e:any) { error.value=e?.response?.data?.error || e?.message || 'Request failed' } finally { busy.value=false } }
const load=async()=>{ await run(()=>adminAPI.accounts.getOllamaCloudUsage(props.account.id)); globalSettings.value=await adminAPI.accounts.getOllamaCloudUsageSettings() }; const save=()=>run(async()=>{ const v=await adminAPI.accounts.saveOllamaCloudUsageSession(props.account.id,session.value); session.value=''; return v }); const refresh=()=>run(()=>adminAPI.accounts.refreshOllamaCloudUsage(props.account.id)); const remove=()=>run(()=>adminAPI.accounts.deleteOllamaCloudUsageSession(props.account.id)); const toggle=(enabled:boolean)=>run(()=>adminAPI.accounts.setOllamaCloudUsageAutoRefresh(props.account.id,enabled)); const saveGlobal=async()=>{ if(!globalSettings.value)return; busy.value=true; try{globalSettings.value=await adminAPI.accounts.updateOllamaCloudUsageSettings(globalSettings.value)}finally{busy.value=false} }; onMounted(()=>{ if(eligible.value) load() })
</script>
