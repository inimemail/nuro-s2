<template>
  <section class="rounded-xl border border-gray-200 bg-gray-50/70 p-4 dark:border-dark-600 dark:bg-dark-800/50 sm:p-5">
    <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
      <label for="registration-blacklist" class="font-medium text-gray-900 dark:text-gray-100">{{ t('upgradeBlacklist.title') }}</label>
      <span class="rounded-full bg-white px-2.5 py-1 text-xs tabular-nums text-gray-500 ring-1 ring-gray-200 dark:bg-dark-700 dark:text-gray-300 dark:ring-dark-600">{{ modelValue.length.toLocaleString() }} / 20,000</span>
    </div>
    <p id="registration-blacklist-help" class="mb-3 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('upgradeBlacklist.help') }}</p>
    <textarea id="registration-blacklist" v-model="text" rows="5" spellcheck="false" aria-describedby="registration-blacklist-help" class="input w-full resize-y font-mono text-sm" :placeholder="t('upgradeBlacklist.placeholder')" />
    <div class="mt-3 flex flex-wrap items-center gap-3">
      <button type="button" class="btn btn-secondary text-sm" :disabled="busy || dirty || !saved.length" @click="preview(0)">{{ t('upgradeBlacklist.preview') }}</button>
      <span v-if="dirty" class="text-xs text-amber-700 dark:text-amber-400">{{ t('upgradeBlacklist.saveFirst') }}</span>
    </div>
    <p v-if="message" class="mt-3 text-sm text-gray-600 dark:text-gray-300" role="status" aria-live="polite">{{ message }}</p>
    <div v-if="page" class="mt-4 overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-600 dark:bg-dark-800">
      <div class="flex items-center justify-between gap-3 border-b border-gray-100 px-3 py-2 dark:border-dark-700">
        <span class="text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('upgradeBlacklist.matches', { count: page.users.length }) }}</span>
        <button v-if="page.has_more" type="button" class="text-xs text-primary-600 hover:underline disabled:opacity-50 dark:text-primary-400" :disabled="busy" @click="preview(page.next_id)">{{ t('upgradeBlacklist.next') }}</button>
      </div>
      <ul class="max-h-48 divide-y divide-gray-100 overflow-auto text-sm dark:divide-dark-700">
        <li v-for="user in page.users" :key="user.id" class="flex items-center justify-between gap-4 px-3 py-2"><span class="truncate">{{ user.email }}</span><span class="shrink-0 font-mono text-xs text-gray-400">#{{ user.id }}</span></li>
      </ul>
      <div v-if="page.users.length" class="border-t border-gray-100 p-3 dark:border-dark-700">
        <label class="flex items-start gap-2 text-xs leading-relaxed text-gray-600 dark:text-gray-300"><input v-model="confirmed" type="checkbox" class="mt-0.5 rounded" />{{ t('upgradeBlacklist.confirm') }}</label>
        <button type="button" class="btn mt-3 bg-red-600 text-sm text-white hover:bg-red-700 focus-visible:ring-2 focus-visible:ring-red-500 disabled:opacity-50" :disabled="busy || !confirmed || dirty" @click="disableUsers">{{ busy ? t('common.loading') : t('upgradeBlacklist.disable') }}</button>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { apiClient } from '@/api/client'

interface Candidate { id: number; email: string; updated_at: string }
interface Preview { revision: string; users: Candidate[]; has_more: boolean; next_id: number }
const props = defineProps<{ modelValue: string[]; saved: string[] }>()
const emit = defineEmits<{ 'update:modelValue': [value: string[]] }>()
const { t } = useI18n()
const draft = ref(props.modelValue.join('\n'))
const splitEmails = (value: string) => value.split(/\r?\n/).map(v => v.trim()).filter(Boolean)
const text = computed({ get: () => draft.value, set: (value: string) => { draft.value = value; emit('update:modelValue', splitEmails(value)) } })
watch(() => props.modelValue, value => { if (JSON.stringify(value) !== JSON.stringify(splitEmails(draft.value))) draft.value = value.join('\n') })
const dirty = computed(() => JSON.stringify(props.modelValue) !== JSON.stringify(props.saved))
const page = ref<Preview | null>(null)
const busy = ref(false)
const confirmed = ref(false)
const message = ref('')
let controller: AbortController | undefined
let batchID = ''
let generation = 0
function createBatchID(): string {
  // LAN HTTP deployments lack randomUUID, but still expose getRandomValues.
  if (globalThis.crypto.randomUUID) return globalThis.crypto.randomUUID()
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6] & 0x0f) | 0x40
  bytes[8] = (bytes[8] & 0x3f) | 0x80
  const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}
watch(() => [props.modelValue, props.saved], () => { generation++; controller?.abort(); busy.value = false; page.value = null; confirmed.value = false })
onUnmounted(() => controller?.abort())
async function preview(after: number) {
  if (busy.value || dirty.value) return
  busy.value = true; message.value = ''; confirmed.value = false
  const current = ++generation
  const active = new AbortController(); controller = active
  try {
    const result = await apiClient.get<Preview>('/admin/users/registration-blacklist/preview', { params: { after_id: after }, signal: active.signal })
    if (current !== generation || active.signal.aborted) return
    batchID = createBatchID()
    page.value = result.data
    if (!page.value.users.length) message.value = t('upgradeBlacklist.empty')
  } catch { if (current === generation && !active.signal.aborted) message.value = t('upgradeBlacklist.failed') }
  finally { if (current === generation) busy.value = false }
}
async function disableUsers() {
  if (busy.value || !confirmed.value || !page.value || dirty.value) return
  busy.value = true; message.value = ''; const current = ++generation
  const active = new AbortController(); controller = active
  try {
    const { data } = await apiClient.post<{ disabled_ids: number[] }>('/admin/users/registration-blacklist/disable', { revision: page.value.revision, batch_id: batchID, users: page.value.users, confirm: true }, { signal: active.signal })
    if (current !== generation || active.signal.aborted) return
    message.value = t('upgradeBlacklist.done', { count: data.disabled_ids.length })
    page.value = null; confirmed.value = false
  } catch { if (current === generation && !active.signal.aborted) message.value = t('upgradeBlacklist.changed') }
  finally { if (current === generation) busy.value = false }
}
</script>
