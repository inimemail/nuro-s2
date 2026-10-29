<template>
  <section v-if="enabled" class="card min-w-0" aria-labelledby="access-keys-title">
    <div class="flex flex-wrap items-start justify-between gap-4 border-b border-gray-100 p-6 dark:border-dark-700">
      <div class="min-w-0 flex-1">
        <div class="flex items-center gap-2"><Icon name="shield" class="text-primary-500" /><h2 id="access-keys-title" class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('accessKeys.title') }}</h2></div>
        <p class="mt-2 max-w-2xl text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('accessKeys.description') }}</p>
      </div>
      <button type="button" class="btn btn-primary btn-sm" :disabled="busy || loading" @click="edit()">{{ t('accessKeys.create') }}</button>
    </div>
    <div class="space-y-4 p-6">
      <p class="rounded-xl bg-primary-50 p-4 text-sm leading-6 text-primary-800 dark:bg-primary-900/20 dark:text-primary-200">{{ t('accessKeys.boundary') }}</p>
      <p v-if="error" role="alert" class="break-words text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      <p v-if="loading" role="status" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">{{ t('common.loading') }}</p>
      <div v-else-if="!keys.length" class="rounded-xl border border-dashed border-gray-200 p-8 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400">{{ t('accessKeys.empty') }}</div>
      <article v-for="key in keys" :key="key.id" class="min-w-0 rounded-xl border border-gray-200 p-4 dark:border-dark-600">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div class="min-w-0"><h3 class="break-words font-medium text-gray-900 dark:text-gray-100">{{ key.name }}</h3><code class="text-xs text-gray-500 dark:text-gray-400">{{ key.prefix }}…</code></div>
          <span class="rounded-full bg-gray-100 px-3 py-1 text-xs font-medium text-gray-700 dark:bg-dark-700 dark:text-gray-200">{{ state(key) }}</span>
        </div>
        <p v-if="key.description" class="mt-2 break-words text-sm text-gray-500 dark:text-gray-400">{{ key.description }}</p>
        <div class="mt-3 flex flex-wrap gap-2"><span v-for="permission in summary(key)" :key="permission" class="max-w-full break-all rounded-md bg-primary-50 px-2 py-1 text-xs text-primary-700 dark:bg-primary-900/20 dark:text-primary-200">{{ permissionLabel(permission) }}</span></div>
        <details class="mt-3 text-sm text-gray-600 dark:text-gray-400"><summary class="cursor-pointer rounded focus-visible:ring-2 focus-visible:ring-primary-500">{{ t('accessKeys.scope') }} · {{ key.policy.rules.length }} {{ t('accessKeys.rules') }}</summary><pre class="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-800">{{ JSON.stringify(key.policy, null, 2) }}</pre></details>
        <dl class="mt-3 grid gap-2 text-xs text-gray-500 dark:text-gray-400 sm:grid-cols-3"><div><dt>{{ t('accessKeys.expiry') }}</dt><dd>{{ date(key.expires_at) }}</dd></div><div><dt>{{ t('accessKeys.lastUsed') }}</dt><dd>{{ date(key.last_used_at) }}</dd></div><div><dt>{{ t('accessKeys.rate') }}</dt><dd>{{ key.rpm }} / min</dd></div></dl>
        <div class="mt-4 flex flex-wrap gap-2">
          <button type="button" class="btn btn-secondary btn-sm" :disabled="busy || !!key.revoked_at" @click="edit(key)">{{ t('common.edit') }}</button>
          <button type="button" class="btn btn-secondary btn-sm" :disabled="busy || !!key.revoked_at" @click="confirmChange(key, 'rotate')">{{ t('accessKeys.rotate') }}</button>
          <button type="button" class="btn btn-secondary btn-sm text-red-600 dark:text-red-400" :disabled="busy || !!key.revoked_at" @click="confirmChange(key, 'revoke')">{{ t('accessKeys.revoke') }}</button>
          <button type="button" class="btn btn-secondary btn-sm" :disabled="busy" @click="confirmChange(key, 'delete')">{{ t('common.delete') }}</button>
        </div>
      </article>
      <div class="flex flex-wrap gap-2">
        <button v-if="nextBeforeID" type="button" class="btn btn-secondary btn-sm" :disabled="busy || loading" @click="load(true)">{{ t('accessKeys.loadMore') }}</button>
        <button type="button" class="btn btn-secondary btn-sm" :disabled="busy || loading" @click="load()">{{ t('common.refresh') }}</button>
      </div>
    </div>
    <BaseDialog trap-focus :show="editor" :title="t(editingID ? 'accessKeys.edit' : 'accessKeys.create')" width="wide" @close="closeEditor">
      <form id="access-key-form" class="space-y-5" @submit.prevent="save">
        <p v-if="formError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ formError }}</p>
        <div class="grid gap-4 sm:grid-cols-2">
          <label class="block text-sm font-medium">{{ t('accessKeys.name') }}<input v-model="draft.name" required maxlength="100" class="input mt-1" :disabled="busy" autocomplete="off" /></label>
          <label class="block text-sm font-medium">{{ t('accessKeys.rate') }}<input v-model.number="draft.rpm" type="number" min="1" max="600" required class="input mt-1" :disabled="busy" /></label>
        </div>
        <label class="block text-sm font-medium">{{ t('accessKeys.descriptionLabel') }}<input v-model="draft.description" maxlength="1000" class="input mt-1" :disabled="busy" /></label>
        <div class="grid gap-4 sm:grid-cols-2">
          <label class="block text-sm font-medium">{{ t('accessKeys.expiry') }}<input v-model="expiry" type="datetime-local" step="1" class="input mt-1" :disabled="busy" /></label>
          <label class="flex items-center gap-2 text-sm"><input v-model="draft.enabled" type="checkbox" class="h-4 w-4" :disabled="busy" />{{ t('accessKeys.enabled') }}</label>
        </div>
        <label class="block text-sm font-medium">{{ t('accessKeys.ip') }}<textarea v-model="ips" rows="2" class="input mt-1" :disabled="busy" :placeholder="t('accessKeys.ipHint')" /></label>
        <fieldset class="space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-600">
          <legend class="px-2 text-sm font-semibold">{{ t('accessKeys.permissions') }}</legend>
          <p class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('accessKeys.ruleHint') }}</p>
          <div class="grid gap-3 sm:grid-cols-2"><label v-for="permission in permissions" :key="permission" class="flex items-start gap-2 text-sm"><input v-model="selected" type="checkbox" :value="permission" class="mt-0.5 h-4 w-4 shrink-0" :disabled="busy || advanced" /><span class="break-words">{{ permissionLabel(permission) }}</span></label></div>
        </fieldset>
        <fieldset class="space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-600" :disabled="busy || advanced">
          <legend class="px-2 text-sm font-semibold">{{ t('accessKeys.scope') }}</legend>
          <label class="flex items-center gap-2 text-sm"><input v-model="all" type="checkbox" class="h-4 w-4" />{{ t('accessKeys.all') }}</label>
          <div class="grid gap-3 sm:grid-cols-3"><label v-for="field in scopeFields" :key="field" class="block text-sm">{{ t(`accessKeys.${field}`) }}<input v-model="scopes[field]" class="input mt-1" :disabled="all" placeholder="12, 34" inputmode="numeric" /></label></div>
          <label class="flex items-center gap-2 text-sm"><input v-model="ungrouped" type="checkbox" class="h-4 w-4" :disabled="all" />{{ t('accessKeys.ungrouped') }}</label>
          <div class="grid gap-3 sm:grid-cols-2"><label class="block text-sm">{{ t('accessKeys.platforms') }}<input v-model="platforms" class="input mt-1" placeholder="openai, grok" /></label><label class="block text-sm">{{ t('accessKeys.types') }}<input v-model="types" class="input mt-1" placeholder="apikey, oauth" /></label></div>
        </fieldset>
        <details :open="advanced" class="rounded-xl border border-gray-200 p-4 dark:border-dark-600"><summary class="cursor-pointer text-sm font-medium">{{ t('accessKeys.advanced') }}</summary><label class="mt-3 flex items-center gap-2 text-sm"><input v-model="advanced" type="checkbox" class="h-4 w-4" :disabled="busy" />{{ t('accessKeys.useAdvanced') }}</label><p class="mt-2 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('accessKeys.advancedHint') }}</p><textarea v-model="policyJSON" rows="10" class="input mt-3 font-mono text-xs" :disabled="busy || !advanced" :aria-label="t('accessKeys.advanced')" spellcheck="false" /></details>
      </form>
      <template #footer><button type="button" class="btn btn-secondary" :disabled="busy" @click="closeEditor">{{ t('common.cancel') }}</button><button type="submit" form="access-key-form" class="btn btn-primary" :disabled="busy">{{ busy ? t('common.saving') : t('common.save') }}</button></template>
    </BaseDialog>
    <BaseDialog trap-focus :show="!!pending" :title="t('accessKeys.confirmTitle')" width="narrow" @close="!busy && (pending = null)"><p class="break-words text-sm leading-6">{{ t('accessKeys.confirmAction', { name: pending?.key.name, action: t(`accessKeys.${pending?.action || 'delete'}`) }) }}</p><p class="mt-3 text-sm text-gray-500 dark:text-gray-400">{{ t('accessKeys.confirmHint') }}</p><template #footer><button type="button" class="btn btn-secondary" :disabled="busy" @click="pending = null">{{ t('common.cancel') }}</button><button type="button" class="btn btn-primary" :disabled="busy" @click="change">{{ t('common.confirm') }}</button></template></BaseDialog>
    <BaseDialog trap-focus :show="!!secret" :title="t('accessKeys.secretTitle')" width="normal" :close-on-escape="false" @close="closeSecret">
      <p v-if="secretError" role="alert" class="mb-3 text-sm text-red-600 dark:text-red-400">{{ secretError }}</p>
      <p class="rounded-xl bg-amber-50 p-4 text-sm leading-6 text-amber-800 dark:bg-amber-900/20 dark:text-amber-200">{{ t('accessKeys.secretHint') }}</p><code class="mt-4 block select-all break-all rounded-xl border border-gray-200 p-4 text-sm dark:border-dark-600">{{ secret }}</code><label class="mt-4 flex items-center gap-2 text-sm"><input v-model="savedSecret" type="checkbox" class="h-4 w-4" />{{ t('accessKeys.savedSecret') }}</label><template #footer><button type="button" class="btn btn-secondary" @click="copySecret">{{ t('common.copy') }}</button><button type="button" class="btn btn-primary" :disabled="!savedSecret || busy" @click="closeSecret">{{ t('common.close') }}</button></template>
    </BaseDialog>
    <TotpStepUpDialog :controller="stepUp" />
  </section>
</template>

<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAccessKeysAPI, type AccessKey, type AccessKeyInput, type AccessRule } from '@/api/admin/adminAccessKeys'
import { useStepUp, isStepUpCancelled } from '@/composables/useStepUp'
import { extractSafeApiErrorMessage, isApiRequestCancelled } from '@/utils/apiError'
import BaseDialog from '@/components/common/BaseDialog.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import Icon from '@/components/icons/Icon.vue'
const { t, te } = useI18n()
const stepUp = useStepUp()
const enabled = ref(false), loading = ref(false), busy = ref(false), editor = ref(false)
const keys = ref<AccessKey[]>([]), permissions = ref<string[]>([]), selected = ref<string[]>([])
const nextBeforeID = ref(0)
const secretError = ref('')
const error = ref(''), formError = ref(''), secret = ref(''), savedSecret = ref(false), editingID = ref(0)
const scopeFields = ['account_ids', 'group_ids', 'proxy_ids'] as const
const scopes = reactive({ account_ids: '', group_ids: '', proxy_ids: '' })
const all = ref(false), ungrouped = ref(false), platforms = ref(''), types = ref(''), ips = ref(''), expiry = ref(''), advanced = ref(false), policyJSON = ref('')
const draft = reactive<AccessKeyInput>({ name: '', description: '', enabled: true, expires_at: null, ip_allowlist: [], policy: { rules: [] }, rpm: 60, revision: 0 })
const pending = ref<{ key: AccessKey; action: 'rotate' | 'revoke' | 'delete' } | null>(null)
let alive = true, generation = 0
let originalExpiry: string | null = null, originalExpiryDisplay = ''
let secretReturnFocus: HTMLElement | null = null
const abort = new AbortController()
function fail(e: unknown) { if (alive && !isApiRequestCancelled(e) && !isStepUpCancelled(e)) error.value = extractSafeApiErrorMessage(e, t('accessKeys.failed')) }
async function load(more = false) {
  const current = ++generation
  loading.value = true
  error.value = ''
  try {
    const page = await adminAccessKeysAPI.list(abort.signal, more ? nextBeforeID.value : 0)
    if (alive && current === generation) {
      keys.value = more ? [...keys.value, ...page.items] : page.items
      nextBeforeID.value = page.next_before_id
    }
  } catch (e) { if (current === generation) fail(e) }
  finally { if (alive && current === generation) loading.value = false }
}
onMounted(async () => { try { const c = await adminAccessKeysAPI.capabilities(abort.signal); if (!alive) return; enabled.value = c.enabled; permissions.value = c.permissions; if (c.enabled) await load() } catch (e) { fail(e) } })
onBeforeUnmount(() => { alive = false; generation++; abort.abort(); secret.value = ''; policyJSON.value = '' })
function permissionLabel(p: string) { const k = `accessKeys.permission.${p.replace(/\./g, '_')}`; return te(k) ? t(k) : p }
function summary(key: AccessKey) { return [...new Set(key.policy.rules.flatMap(r => r.permissions))] }
function date(value: string | null | undefined) { return value ? new Date(value).toLocaleString() : '—' }
function state(key: AccessKey) { return t(key.revoked_at ? 'accessKeys.revoked' : key.expires_at && new Date(key.expires_at).getTime() <= Date.now() ? 'accessKeys.expired' : key.enabled ? 'accessKeys.enabled' : 'accessKeys.disabled') }
function edit(key?: AccessKey) {
  if (busy.value) return
  secretReturnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
  editingID.value = key?.id || 0; formError.value = ''
  Object.assign(draft, { name: key?.name || '', description: key?.description || '', enabled: key?.enabled ?? true, rpm: key?.rpm || 60, revision: key?.revision || 0 })
  ips.value = key?.ip_allowlist.join('\n') || ''
  originalExpiry = key?.expires_at || null
  const expiresAt = originalExpiry ? new Date(originalExpiry) : null
  originalExpiryDisplay = expiresAt ? new Date(expiresAt.getTime() - expiresAt.getTimezoneOffset() * 60000).toISOString().slice(0, 19) : ''
  expiry.value = originalExpiryDisplay
  const rule = key?.policy.rules[0]; selected.value = [...(rule?.permissions || ['accounts.read'])]; all.value = rule?.all_resources || false; ungrouped.value = rule?.allow_ungrouped || false
  for (const f of scopeFields) scopes[f] = rule?.[f]?.join(', ') || ''
  platforms.value = rule?.platforms?.join(', ') || ''; types.value = rule?.account_types?.join(', ') || ''
  advanced.value = (key?.policy.rules.length || 0) > 1; policyJSON.value = JSON.stringify(key?.policy || { rules: [{ permissions: ['accounts.read'], account_ids: [] }] }, null, 2); editor.value = true
}
function closeEditor() { if (!busy.value) editor.value = false }
const split = (value: string) => value.split(/[\s,]+/).filter(Boolean)
function ids(value: string) { const values = split(value); if (values.some(v => !/^[1-9]\d*$/.test(v) || !Number.isSafeInteger(Number(v)))) throw new Error(t('accessKeys.invalidIDs')); return [...new Set(values.map(Number))] }
async function save() {
  if (busy.value) return
  formError.value = ''; error.value = ''
  let input: AccessKeyInput
  try {
    const rule: AccessRule = { permissions: [...selected.value], all_resources: all.value, allow_ungrouped: ungrouped.value, platforms: split(platforms.value), account_types: split(types.value) }
    for (const f of scopeFields) rule[f] = ids(scopes[f])
    const policy = advanced.value ? JSON.parse(policyJSON.value) : { rules: [rule] }
    if (!Array.isArray(policy?.rules) || !policy.rules.length || policy.rules.length > 32) throw new Error(t('accessKeys.invalidPolicy'))
    input = { ...draft, policy, expires_at: expiry.value === originalExpiryDisplay ? originalExpiry : expiry.value ? new Date(expiry.value).toISOString() : null, ip_allowlist: split(ips.value) }
  } catch (e) { formError.value = extractSafeApiErrorMessage(e, t('accessKeys.invalidPolicy')); return }
  busy.value = true
  try {
    if (editingID.value) await stepUp.run(() => adminAccessKeysAPI.update(editingID.value, input))
    else { const result = await stepUp.run(() => adminAccessKeysAPI.create(input)); if (alive) { secret.value = result.secret; savedSecret.value = false; secretError.value = '' } }
    if (alive) { editor.value = false; await load() }
  } catch (e) { if (alive && !isStepUpCancelled(e)) formError.value = extractSafeApiErrorMessage(e, t('accessKeys.failed')) } finally { if (alive) busy.value = false }
}
function confirmChange(key: AccessKey, action: 'rotate' | 'revoke' | 'delete') {
  secretReturnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
  pending.value = { key, action }
}
async function change() {
  if (!pending.value || busy.value) return
  const { key, action } = pending.value; busy.value = true; error.value = ''
  try { const result = await stepUp.run(() => adminAccessKeysAPI.change(key.id, key.revision, action)); if (alive) { pending.value = null; if (result.secret) { secret.value = result.secret; savedSecret.value = false; secretError.value = '' } await load() } } catch (e) { fail(e); if (alive) pending.value = null } finally { if (alive) busy.value = false }
}
function closeSecret() {
  if (!savedSecret.value || busy.value) return
  secret.value = ''; savedSecret.value = false; secretError.value = ''
  const target = secretReturnFocus
  void nextTick(() => { if (alive && target?.isConnected) target.focus() })
}
async function copySecret() { secretError.value = ''; try { await navigator.clipboard.writeText(secret.value) } catch { if (alive) secretError.value = t('accessKeys.copyFailed') } }
</script>
