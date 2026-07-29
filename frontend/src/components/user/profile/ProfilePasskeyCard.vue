<template>
  <section class="card overflow-hidden">
    <header class="flex flex-wrap items-start justify-between gap-3 border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <div>
        <h2 class="text-lg font-medium text-gray-900 dark:text-white">{{ t('profile.passkey.title') }}</h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('profile.passkey.description') }}</p>
      </div>
      <button v-if="enabled && supported && !showAdd" type="button" class="btn btn-primary" :disabled="busy" @click="showAdd = true">
        <Icon name="plus" size="sm" class="mr-1.5" />{{ t('profile.passkey.add') }}
      </button>
    </header>

    <div class="px-6 py-5">
      <p v-if="!enabled" class="text-sm text-gray-500 dark:text-gray-400">{{ t('profile.passkey.featureDisabled') }}</p>
      <p v-if="enabled && !supported" class="text-sm text-amber-600 dark:text-amber-400">{{ t('profile.passkey.unsupported') }}</p>

      <form v-if="enabled && supported && showAdd" class="mb-5 border-b border-gray-100 pb-5 dark:border-dark-700" @submit.prevent="addPasskey">
        <div class="grid gap-3 sm:grid-cols-2">
          <label class="block">
            <span class="input-label">{{ t('profile.passkey.name') }}</span>
            <input v-model="newName" class="input" maxlength="100" :placeholder="t('profile.passkey.namePlaceholder')" autofocus />
          </label>
          <label class="block">
            <span class="input-label">{{ t('profile.currentPassword') }}</span>
            <input v-model="newPassword" type="password" autocomplete="current-password" class="input" :placeholder="t('profile.passkey.passwordPlaceholder')" />
          </label>
        </div>
        <div class="mt-3 flex justify-end gap-2">
          <button type="button" class="btn btn-secondary" :disabled="busy" @click="cancelAdd">{{ t('common.cancel') }}</button>
          <button type="submit" class="btn btn-primary" :disabled="busy || !newPassword">{{ busy ? t('common.processing') : t('profile.passkey.continue') }}</button>
        </div>
      </form>

      <div v-if="enabled && loading" class="flex h-24 items-center justify-center">
        <span class="h-7 w-7 animate-spin rounded-full border-2 border-primary-200 border-t-primary-600"></span>
      </div>
      <div v-else-if="enabled && credentials.length === 0" class="border border-dashed border-gray-200 px-4 py-7 text-center text-sm text-gray-500 dark:border-dark-700 dark:text-gray-400">
        {{ t('profile.passkey.empty') }}
      </div>
      <div v-else-if="enabled" class="divide-y divide-gray-100 dark:divide-dark-700">
        <div v-for="credential in credentials" :key="credential.id" class="flex min-h-[64px] items-center gap-3 py-3">
          <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-primary-50 text-primary-600 dark:bg-primary-900/20 dark:text-primary-400">
            <Icon name="key" size="md" />
          </div>
          <div class="min-w-0 flex-1">
            <div v-if="editingId === credential.id" class="flex max-w-md gap-2">
              <input v-model="editingName" class="input h-9 min-w-0" maxlength="100" @keydown.enter.prevent="saveRename(credential)" @keydown.esc="cancelRename" />
              <button type="button" class="btn btn-primary btn-sm" :disabled="busy || !editingName.trim()" @click="saveRename(credential)">{{ t('common.save') }}</button>
            </div>
            <template v-else>
              <div class="flex min-w-0 items-center gap-2">
                <span class="truncate font-medium text-gray-900 dark:text-white">{{ credential.name }}</span>
                <span v-if="credential.backup" class="shrink-0 rounded-md bg-green-50 px-1.5 py-0.5 text-xs text-green-700 dark:bg-green-900/20 dark:text-green-300">{{ t('profile.passkey.synced') }}</span>
              </div>
              <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
                {{ t('profile.passkey.createdAt', { date: formatDate(credential.created_at) }) }}
                <template v-if="credential.last_used_at"> · {{ t('profile.passkey.lastUsed', { date: formatDate(credential.last_used_at) }) }}</template>
              </p>
            </template>
          </div>
          <div v-if="editingId !== credential.id" class="flex shrink-0 gap-1">
            <button type="button" class="rounded-lg p-2 text-gray-400 hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-dark-700 dark:hover:text-white" :title="t('common.edit')" :aria-label="t('common.edit')" :disabled="busy" @click="beginRename(credential)"><Icon name="edit" size="sm" /></button>
            <button type="button" class="rounded-lg p-2 text-gray-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950/20 dark:hover:text-red-300" :title="t('common.delete')" :aria-label="t('common.delete')" :disabled="busy" @click="deleteTarget = credential"><Icon name="trash" size="sm" /></button>
          </div>
        </div>
      </div>
    </div>

    <div v-if="deleteTarget" class="fixed inset-0 z-50 flex items-center justify-center p-4">
      <button type="button" class="absolute inset-0 bg-black/50" :aria-label="t('common.close')" @click="closeDelete"></button>
      <form class="relative w-full max-w-md rounded-lg bg-white p-6 shadow-xl dark:bg-dark-800" @submit.prevent="confirmDelete">
        <h3 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('profile.passkey.deleteTitle') }}</h3>
        <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ t('profile.passkey.deleteConfirm', { name: deleteTarget.name }) }}</p>
        <label class="mt-4 block">
          <span class="input-label">{{ t('profile.currentPassword') }}</span>
          <input v-model="deletePassword" type="password" autocomplete="current-password" class="input" :placeholder="t('profile.passkey.passwordPlaceholder')" autofocus />
        </label>
        <div class="mt-5 flex justify-end gap-2">
          <button type="button" class="btn btn-secondary" :disabled="busy" @click="closeDelete">{{ t('common.cancel') }}</button>
          <button type="submit" class="btn btn-danger" :disabled="busy || !deletePassword">{{ busy ? t('common.processing') : t('common.delete') }}</button>
        </div>
      </form>
    </div>
  </section>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { passkeyAPI, type PasskeyCredentialSummary } from '@/api'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores/app'

const props = defineProps<{ enabled: boolean }>()
const { t } = useI18n()
const appStore = useAppStore()
const supported = passkeyAPI.isSupported()
const loading = ref(false)
const busy = ref(false)
const showAdd = ref(false)
const newName = ref('')
const newPassword = ref('')
const credentials = ref<PasskeyCredentialSummary[]>([])
const editingId = ref<number | null>(null)
const editingName = ref('')
const deleteTarget = ref<PasskeyCredentialSummary | null>(null)
const deletePassword = ref('')

function errorMessage(error: unknown, fallback: string): string {
  const value = (error as { message?: string }).message
  return typeof value === 'string' && value ? value : fallback
}

async function loadCredentials(): Promise<void> {
  if (!props.enabled) { credentials.value = []; return }
  loading.value = true
  try { credentials.value = await passkeyAPI.list() }
  catch (error) {
    if ((error as { reason?: string }).reason !== 'PASSKEY_DISABLED') appStore.showError(t('profile.passkey.loadFailed'))
  } finally { loading.value = false }
}

async function addPasskey(): Promise<void> {
  if (!newPassword.value) return
  busy.value = true
  try {
    await passkeyAPI.register(newName.value.trim(), newPassword.value)
    appStore.showSuccess(t('profile.passkey.added'))
    cancelAdd()
    await loadCredentials()
  } catch (error) {
    if (!(error instanceof DOMException && error.name === 'NotAllowedError')) appStore.showError(errorMessage(error, t('profile.passkey.addFailed')))
  } finally { busy.value = false }
}

function cancelAdd(): void { showAdd.value = false; newName.value = ''; newPassword.value = '' }
function beginRename(item: PasskeyCredentialSummary): void { editingId.value = item.id; editingName.value = item.name }
function cancelRename(): void { editingId.value = null; editingName.value = '' }
async function saveRename(item: PasskeyCredentialSummary): Promise<void> {
  const name = editingName.value.trim()
  if (!name) return
  busy.value = true
  try { await passkeyAPI.rename(item.id, name); item.name = name; cancelRename(); appStore.showSuccess(t('profile.passkey.renamed')) }
  catch { appStore.showError(t('profile.passkey.renameFailed')) }
  finally { busy.value = false }
}
function closeDelete(): void { deleteTarget.value = null; deletePassword.value = '' }
async function confirmDelete(): Promise<void> {
  const item = deleteTarget.value
  if (!item || !deletePassword.value) return
  busy.value = true
  try { await passkeyAPI.remove(item.id, deletePassword.value); credentials.value = credentials.value.filter(value => value.id !== item.id); closeDelete(); appStore.showSuccess(t('profile.passkey.deleted')) }
  catch (error) { appStore.showError(errorMessage(error, t('profile.passkey.deleteFailed'))) }
  finally { busy.value = false }
}
function formatDate(value: string): string { return new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' }).format(new Date(value)) }

watch(() => props.enabled, () => { void loadCredentials() }, { immediate: true })
</script>
