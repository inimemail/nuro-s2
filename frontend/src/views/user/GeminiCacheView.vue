<template>
  <AppLayout>
    <div class="mx-auto max-w-7xl space-y-6">
      <section class="relative overflow-hidden rounded-2xl border border-primary-100 bg-gradient-to-br from-primary-50 via-white to-cyan-50 p-6 dark:border-primary-900/40 dark:from-primary-950/30 dark:via-dark-800 dark:to-dark-800 sm:p-8">
        <div class="flex flex-col justify-between gap-6 sm:flex-row sm:items-center">
          <div class="max-w-2xl space-y-3">
            <span class="inline-flex items-center gap-2 rounded-full bg-white/80 px-3 py-1 text-xs font-semibold tracking-wide text-primary-700 dark:bg-primary-900/30 dark:text-primary-300"><Icon name="database" size="sm" /> GEMINI · CONTEXT CACHE</span>
            <h1 class="text-2xl font-semibold tracking-tight text-gray-900 dark:text-white sm:text-3xl">{{ t('geminiCache.title') }}</h1>
            <p class="text-sm leading-6 text-gray-600 dark:text-gray-400">{{ t('geminiCache.description') }}</p>
          </div>
          <button class="btn btn-primary shrink-0" :disabled="!selectedKey || busy || uncertain" @click="openCreate"><Icon name="plus" size="md" />{{ t('geminiCache.create') }}</button>
        </div>
        <div class="mt-6 grid gap-3 sm:grid-cols-3">
          <div v-for="stat in stats" :key="stat.label" class="rounded-xl border border-white/70 bg-white/70 px-4 py-3 dark:border-dark-600 dark:bg-dark-800/80">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ stat.label }}</p><p class="mt-1 text-xl font-semibold tabular-nums text-gray-900 dark:text-white">{{ stat.value }}</p>
          </div>
        </div>
      </section>

      <section class="rounded-2xl border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800">
        <div class="flex flex-wrap items-end justify-between gap-4">
          <div class="w-full max-w-md"><label class="mb-2 block text-sm font-medium">{{ t('geminiCache.key') }}</label><Select v-model="keyId" :options="keyOptions" :placeholder="t('geminiCache.selectKey')" :disabled="busy || uncertain" /></div>
          <button class="btn btn-secondary" :disabled="loading || busy || !selectedKey" @click="load(false)"><Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />{{ t('common.refresh') }}</button>
        </div>
        <p class="mt-3 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('geminiCache.keyHint') }}</p>
        <div v-if="keyPages > 1" class="mt-3 flex items-center justify-end gap-3 text-xs text-gray-500"><button class="btn btn-secondary" :disabled="busy || uncertain || keyPage <= 1" @click="loadKeys(keyPage - 1)">{{ t('geminiCache.previousKeys') }}</button><span class="tabular-nums">{{ keyPage }} / {{ keyPages }}</span><button class="btn btn-secondary" :disabled="busy || uncertain || keyPage >= keyPages" @click="loadKeys(keyPage + 1)">{{ t('geminiCache.nextKeys') }}</button></div>
      </section>
      <section v-if="pendingOperation && uncertain" class="rounded-xl border border-amber-200 bg-amber-50 p-4 dark:border-amber-900 dark:bg-amber-950/20"><p class="text-sm leading-6 text-amber-800 dark:text-amber-300">{{ t('geminiCache.pendingHint') }}</p><p class="mt-2 break-all font-mono text-xs text-amber-700 dark:text-amber-400">{{ pendingOperation.id }}</p><button class="btn btn-secondary mt-3" :disabled="busy" @click="checkPending">{{ t('geminiCache.checkPending') }}</button><button v-if="pendingNotRecorded" class="btn btn-secondary ml-3 mt-3" :disabled="busy" @click="cancelUnrecorded">{{ t('geminiCache.cancelUnrecorded') }}</button></section>
      <div v-if="notice" role="status" class="rounded-xl border border-amber-200 bg-amber-50 p-4 text-sm leading-6 text-amber-800 dark:border-amber-800 dark:bg-amber-900/20 dark:text-amber-200">{{ notice }}</div>
      <div v-if="error" role="alert" class="flex items-center justify-between gap-3 rounded-xl border border-red-200 bg-red-50 p-4 text-sm text-red-700 dark:border-red-900 dark:bg-red-950/30 dark:text-red-300"><span>{{ error }}</span><button class="btn btn-secondary" :disabled="loading" @click="load(false)">{{ t('common.retry') }}</button></div>
      <div v-if="loading && !items.length" class="grid gap-4 md:grid-cols-2 xl:grid-cols-3" aria-busy="true"><div v-for="n in 3" :key="n" class="h-64 animate-pulse rounded-2xl bg-gray-100 dark:bg-dark-700" /></div>
      <div v-else-if="!items.length && !error" class="rounded-2xl border border-dashed border-gray-300 p-12 text-center dark:border-dark-600"><Icon name="database" size="xl" class="mx-auto text-gray-400" /><h2 class="mt-4 font-medium">{{ t('geminiCache.empty') }}</h2><p class="mt-2 text-sm text-gray-500">{{ keys.length ? t('geminiCache.emptyHint') : t('geminiCache.noKeys') }}</p></div>
      <div v-else class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <article v-for="item in items" :key="item.name" class="flex flex-col rounded-2xl border border-gray-200 bg-white p-5 shadow-sm transition-shadow hover:shadow-md dark:border-dark-700 dark:bg-dark-800">
          <div class="flex items-start justify-between gap-3"><div class="min-w-0"><h2 class="truncate font-semibold text-gray-900 dark:text-white" :title="item.displayName">{{ item.displayName || t('geminiCache.unnamed') }}</h2><p class="mt-1 break-all text-xs text-gray-500">{{ item.model.replace('models/', '') }}</p></div><span :class="statusClass(item)" class="shrink-0 rounded-full px-2.5 py-1 text-xs font-medium">{{ t(`geminiCache.states.${cacheStatus(item, now)}`) }}</span></div>
          <div class="my-5 grid grid-cols-2 gap-4"><div><p class="text-xs text-gray-500">{{ t('geminiCache.tokens') }}</p><p class="mt-1 text-lg font-semibold tabular-nums">{{ item.usageMetadata.totalTokenCount.toLocaleString() }}</p></div><div><p class="text-xs text-gray-500">{{ t('geminiCache.remaining') }}</p><p class="mt-1 font-mono text-lg tabular-nums">{{ cacheTimeLeft(item.expireTime, now) }}</p></div></div>
          <div class="space-y-2 rounded-xl bg-gray-50 p-3 text-xs dark:bg-dark-900/50"><div class="flex justify-between gap-2"><span class="text-gray-500">{{ t('geminiCache.settled') }}</span><span class="font-mono">{{ cacheMoney(item.billing?.settledAmount) }}</span></div><div class="flex justify-between gap-2"><span class="text-gray-500">{{ t('geminiCache.reserved') }}</span><span class="font-mono">{{ cacheMoney(item.billing?.reservedAmount) }}</span></div><div class="flex flex-wrap justify-between gap-2 text-gray-500"><span>{{ t('geminiCache.inputCost') }} {{ cacheMoney(item.billing?.inputCost) }}</span><span>{{ t('geminiCache.storageCost') }} {{ cacheMoney(item.billing?.storageCost) }}</span></div><p class="text-gray-500">{{ t('geminiCache.expires') }} {{ formatDate(item.expireTime) }}</p></div>
          <div class="mt-4 flex items-center gap-2"><code class="min-w-0 flex-1 truncate text-xs text-gray-500" :title="item.name">{{ item.name }}</code><button class="rounded-lg p-2 text-gray-500 hover:bg-gray-100 dark:hover:bg-dark-700" :aria-label="t('geminiCache.copy')" @click="copy(item.name)"><Icon name="copy" size="sm" /></button></div>
          <div class="mt-4 flex gap-2 border-t border-gray-100 pt-4 dark:border-dark-700"><button class="btn btn-secondary flex-1" :disabled="busy || uncertain || !cacheIsActive(item, now)" @click="openRenew(item)"><Icon name="clock" size="sm" />{{ t('geminiCache.renew') }}</button><button class="btn btn-secondary text-red-600 dark:text-red-400" :disabled="busy || uncertain || item.state !== 'active'" @click="openDelete(item)"><Icon name="trash" size="sm" />{{ t('common.delete') }}</button></div>
          <p v-if="item.state === 'reconcile_required' || item.state === 'pending'" class="mt-3 text-xs leading-5 text-amber-600 dark:text-amber-400">{{ t('geminiCache.pendingHint') }}</p>
        </article>
      </div>
      <button v-if="nextPage" class="btn btn-secondary mx-auto flex" :disabled="loading || busy" @click="load(true)">{{ t('geminiCache.more') }}</button>
      <p class="text-xs leading-6 text-gray-500 dark:text-gray-400">{{ t('geminiCache.policy') }}</p>
    </div>

    <BaseDialog :show="dialog !== null" :title="dialogTitle" :width="dialog === 'create' ? 'wide' : 'normal'" :close-on-escape="!busy && !uncertain" @close="closeDialog">
      <form id="gemini-cache-form" class="space-y-5" @submit.prevent="submit">
        <template v-if="dialog === 'create'">
          <div class="grid gap-4 sm:grid-cols-2"><div><label class="mb-2 block text-sm font-medium" for="cache-name">{{ t('geminiCache.name') }}</label><input id="cache-name" v-model="displayName" maxlength="128" class="input" :disabled="busy || uncertain" :placeholder="t('geminiCache.namePlaceholder')" /></div><div><label class="mb-2 block text-sm font-medium" for="cache-model">{{ t('geminiCache.model') }}</label><input id="cache-model" v-model="model" required class="input" :disabled="busy || uncertain" placeholder="gemini-3.6-flash" /></div></div>
          <div><label class="mb-2 block text-sm font-medium" for="cache-content">{{ t('geminiCache.content') }}</label><textarea id="cache-content" v-model="content" required rows="9" class="input resize-y font-mono text-sm" :disabled="busy || uncertain" :placeholder="t('geminiCache.contentPlaceholder')" /><p class="mt-2 text-xs text-gray-500">{{ t('geminiCache.contentHint') }}</p></div>
        </template>
        <template v-if="dialog === 'create' || dialog === 'renew'">
          <div><label class="mb-2 block text-sm font-medium" for="cache-hours">{{ dialog === 'renew' ? t('geminiCache.extendHours') : t('geminiCache.hours') }}</label><input id="cache-hours" v-model.number="hours" type="number" min="0.016667" max="24" step="any" required class="input max-w-48" :disabled="busy || uncertain" /><p class="mt-2 text-xs text-gray-500">{{ t('geminiCache.ttlHint') }}</p></div>
          <div v-if="quote" class="rounded-xl border border-primary-200 bg-primary-50 p-4 dark:border-primary-900 dark:bg-primary-950/20"><div class="flex justify-between"><span class="text-sm font-medium">{{ t('geminiCache.estimate') }}</span><strong class="font-mono text-primary-700 dark:text-primary-300">{{ cacheMoney(quote.amount) }}</strong></div><p class="mt-2 text-xs text-gray-500">{{ quote.tokens.toLocaleString() }} tokens · {{ t('geminiCache.inputCost') }} {{ cacheMoney(quote.inputCost) }} · {{ t('geminiCache.storageCost') }} {{ cacheMoney(quote.storageCost) }}</p><p class="mt-2 text-xs text-gray-500">{{ t('geminiCache.quoteHint') }}</p></div>
          <p v-if="dialog === 'renew'" class="rounded-xl bg-primary-50 p-4 text-sm leading-6 dark:bg-primary-950/20">{{ t('geminiCache.renewHint') }}</p>
        </template>
        <p v-if="dialog === 'delete'" class="text-sm leading-6 text-gray-600 dark:text-gray-400">{{ t('geminiCache.deleteHint', { name: target?.displayName || target?.name }) }}</p>
        <p v-if="formError" role="alert" class="rounded-xl bg-amber-50 p-3 text-sm leading-6 text-amber-800 dark:bg-amber-950/30 dark:text-amber-300">{{ formError }}</p>
      </form>
      <template #footer><div class="flex flex-wrap justify-end gap-3"><button class="btn btn-secondary" :disabled="busy || uncertain" @click="closeDialog">{{ t('common.cancel') }}</button><button v-if="dialog === 'create' || dialog === 'renew'" class="btn btn-secondary" :disabled="busy || uncertain || (dialog === 'create' && !createBody)" @click="preview">{{ t('geminiCache.preview') }}</button><button class="btn" :class="dialog === 'delete' ? 'btn-danger' : 'btn-primary'" form="gemini-cache-form" type="submit" :disabled="busy || ((dialog === 'create' || dialog === 'renew') && !quote) || (dialog === 'create' && !createBody)">{{ busy ? t('geminiCache.processing') : dialog === 'create' ? t('geminiCache.confirmCreate') : dialog === 'renew' ? t('geminiCache.confirmRenew') : t('common.delete') }}</button></div></template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import axios from 'axios'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import { cancelUnrecordedCacheOperation, cacheOperationStatus, quoteRenewCache, cacheErrorKind, createCache, deleteCache, listCaches, quoteCache, renewCache, listCacheKeys, type GeminiCacheKey, type GeminiCachedContent, type GeminiCacheQuote } from '@/api/geminiCache'
import { cacheOperationID, cacheCreateBody, cacheIsActive, cacheMoney, cacheStatus, cacheTimeLeft } from '@/utils/geminiExplicitCache'
import { useAuthStore } from '@/stores/auth'
import { readPendingCacheOperation, savePendingCacheOperation, clearPendingCacheOperation, type PendingCacheOperation } from '@/utils/geminiCachePending'
const auth = useAuthStore()
const pendingOperation = ref<PendingCacheOperation | null>(null), pendingNotRecorded = ref(false)
const { t, locale } = useI18n()
const keys = ref<GeminiCacheKey[]>([]), keyId = ref<number | null>(null), items = ref<GeminiCachedContent[]>([])
const nextPage = ref(''), loading = ref(false), busy = ref(false), error = ref(''), notice = ref(''), now = ref(Date.now())
const uncertain = ref(false), keyPage = ref(1), keyPages = ref(1)
const dialog = ref<'create' | 'renew' | 'delete' | null>(null), target = ref<GeminiCachedContent | null>(null)
const model = ref('gemini-3.6-flash'), displayName = ref(''), content = ref(''), hours = ref(1), formError = ref(''), quote = ref<GeminiCacheQuote | null>(null)
const selectedKey = computed(() => keys.value.find(k => k.id === keyId.value))
const keyOptions = computed(() => keys.value.map(k => ({ value: k.id, label: `${k.name} · ${k.group?.name || 'Gemini'}` })))
const createBody = computed(() => cacheCreateBody(model.value, content.value, hours.value, displayName.value))
const dialogTitle = computed(() => t(`geminiCache.${dialog.value === 'create' ? 'create' : dialog.value === 'renew' ? 'renew' : 'deleteTitle'}`))
const stats = computed(() => [{ label: t('geminiCache.active'), value: items.value.filter(i => cacheIsActive(i, now.value)).length }, { label: t('geminiCache.totalTokens'), value: items.value.filter(i => cacheIsActive(i, now.value)).reduce((n, i) => n + i.usageMetadata.totalTokenCount, 0).toLocaleString() }, { label: t('geminiCache.pending'), value: items.value.filter(i => i.state === 'pending' || i.state === 'reconcile_required').length }])
let keyController: AbortController | undefined, listController: AbortController | undefined, operationController: AbortController | undefined, interval: ReturnType<typeof setInterval> | undefined, disposed = false
let mutationId = '', mutationFingerprint = '', mutationExpiry = ''
function statusClass(item: GeminiCachedContent) { const status = cacheStatus(item, now.value); return status === 'active' ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/20 dark:text-emerald-300' : status === 'expired' ? 'bg-gray-100 text-gray-500 dark:bg-dark-700 dark:text-gray-400' : 'bg-amber-50 text-amber-700 dark:bg-amber-900/20 dark:text-amber-300' }
function formatDate(value: string) { const d = new Date(value); return Number.isFinite(d.getTime()) ? d.toLocaleString(locale.value) : '—' }
async function copy(name: string) { try { await navigator.clipboard.writeText(name); notice.value = t('geminiCache.copied') } catch { notice.value = t('geminiCache.copyFailed') } }
async function load(append = false) {
  const key = selectedKey.value
  if (!key) return
  listController?.abort(); const controller = new AbortController(); listController = controller; loading.value = true; error.value = ''
  try { const result = await listCaches(key.id, append ? nextPage.value : '', controller.signal); if (disposed || controller.signal.aborted || selectedKey.value?.id !== key.id) return; items.value = append ? [...items.value, ...result.cachedContents.filter(i => !items.value.some(old => old.name === i.name))] : result.cachedContents; nextPage.value = result.nextPageToken || '' }
  catch (e) { if (!axios.isCancel(e) && !disposed && !controller.signal.aborted && selectedKey.value?.id === key.id) error.value = t(`geminiCache.errors.${cacheErrorKind(e)}`) }
  finally { if (listController === controller) loading.value = false }
}
watch(keyId, () => { operationController?.abort(); closeDialog(); listController?.abort(); items.value = []; nextPage.value = ''; error.value = ''; notice.value = ''; void load() })
watch([model, content, hours, displayName], () => { quote.value = null })
function resetMutation() { mutationId = ''; mutationFingerprint = ''; mutationExpiry = '' }
function openCreate() { if (uncertain.value) return; dialog.value = 'create'; target.value = null; quote.value = null; formError.value = ''; resetMutation() }
function openRenew(item: GeminiCachedContent) { if (uncertain.value) return; quote.value = null; dialog.value = 'renew'; target.value = item; hours.value = 1; formError.value = ''; resetMutation() }
function openDelete(item: GeminiCachedContent) { if (uncertain.value) return; quote.value = null; dialog.value = 'delete'; target.value = item; formError.value = ''; resetMutation() }
function closeDialog() { if (busy.value || uncertain.value) return; dialog.value = null; content.value = ''; quote.value = null; resetMutation() }
async function preview() {
  const key = selectedKey.value, body = createBody.value, action = dialog.value, item = target.value
  if (!key || busy.value || uncertain.value || (action === 'create' && !body)) return
  if (action === 'renew' && (!item || !Number.isFinite(hours.value) || hours.value <= 0 || Date.parse(item.expireTime) + hours.value * 3600000 > Date.now() + 86400000)) { formError.value = t('geminiCache.errors.ttl'); return }
  const form = JSON.stringify([body, hours.value, item?.name, action])
  const controller = new AbortController(); operationController = controller
  busy.value = true; formError.value = ''
  try {
    const expiry = item ? new Date(Date.parse(item.expireTime) + hours.value * 3600000).toISOString() : ''
    const result = action === 'renew' && item ? await quoteRenewCache(key.id, item.name, expiry, controller.signal) : await quoteCache(key.id, body!, controller.signal)
    if (!disposed && !controller.signal.aborted && selectedKey.value?.id === key.id && JSON.stringify([createBody.value, hours.value, target.value?.name, dialog.value]) === form) quote.value = result
  } catch (e) { if (!disposed && !controller.signal.aborted && !axios.isCancel(e)) formError.value = t(`geminiCache.errors.${cacheErrorKind(e)}`) }
  finally { if (!disposed && operationController === controller) busy.value = false }
}
function ownsPending(pending: PendingCacheOperation) {
  synchronizePending()
  return pendingOperation.value?.id === pending.id
}
function releasePending(pending: PendingCacheOperation) {
  if (!ownsPending(pending)) return false
  if (auth.user) clearPendingCacheOperation(auth.user.id, pending.id)
  pendingOperation.value = null; pendingNotRecorded.value = false; uncertain.value = false
  return true
}
async function checkPending() {
  const pending = pendingOperation.value
  if (!pending || busy.value) return
  busy.value = true; error.value = ''
  try {
    const status = await cacheOperationStatus(pending.keyID, pending.id, pending.kind)
    if (disposed || !ownsPending(pending)) return
    pendingNotRecorded.value = status.state === 'not_recorded'
    if (status.state === 'settled' || status.state === 'released') { if (!releasePending(pending)) return; dialog.value = null; content.value = ''; quote.value = null; resetMutation(); if (!keys.value.length) { busy.value = false; void loadKeys() } notice.value = t(status.state === 'settled' ? 'geminiCache.success' : 'geminiCache.errors.failed'); await load() }
    else notice.value = t('geminiCache.pendingHint')
  } catch { if (!disposed && ownsPending(pending)) notice.value = t('geminiCache.pendingCheckHint') }
  finally { if (!disposed) busy.value = false }
}
async function cancelUnrecorded() {
  const pending = pendingOperation.value
  if (!pending || busy.value || !pendingNotRecorded.value) return
  busy.value = true
  try { await cancelUnrecordedCacheOperation(pending.keyID,pending.id,pending.kind); if (!disposed && releasePending(pending)) { dialog.value=null; resetMutation(); busy.value=false; await loadKeys() } }
  catch { if (!disposed && ownsPending(pending)) { pendingNotRecorded.value=false; notice.value=t('geminiCache.pendingHint') } }
  finally { if (!disposed) busy.value=false }
}
async function submit() {
  const key = selectedKey.value, action = dialog.value, item = target.value, body = createBody.value
  if (!key || !action || busy.value || ((action === 'create' || action === 'renew') && !quote.value) || (action === 'create' && !body)) return
  if (action === 'renew' && (!item || !Number.isFinite(hours.value) || hours.value <= 0 || Date.parse(item.expireTime) + hours.value * 3600000 > Date.now() + 86400000)) { formError.value = t('geminiCache.errors.ttl'); return }
  const fingerprint = JSON.stringify([key.id, action, action === 'create' ? body : [item?.name, hours.value]])
  if (fingerprint !== mutationFingerprint) { mutationId = cacheOperationID(); mutationFingerprint = fingerprint; mutationExpiry = item ? new Date(Date.parse(item.expireTime) + hours.value * 3600000).toISOString() : '' }
  const userID = auth.user?.id
  if (!userID) return
  const saved = readPendingCacheOperation(userID)
  if (saved && saved.id !== mutationId) { pendingOperation.value = saved; uncertain.value = true; dialog.value = null; notice.value = t('geminiCache.pendingHint'); return }
  const pending: PendingCacheOperation = saved ?? { keyID: key.id, id: mutationId, kind: action === 'renew' ? 'patch' : action, createdAt: Date.now() }
  try { savePendingCacheOperation(userID, pending) } catch { formError.value = t('geminiCache.storageUnavailable'); return }
  pendingOperation.value = pending
  busy.value = true; formError.value = ''; operationController = new AbortController()
  try {
    if (action === 'create' && body) await createCache(key.id, body, mutationId, operationController.signal)
    else if (action === 'renew' && item) await renewCache(key.id, item.name, mutationExpiry, mutationId, operationController.signal)
    else if (action === 'delete' && item) await deleteCache(key.id, item.name, mutationId, operationController.signal)
    if (!disposed && releasePending(pending)) { dialog.value = null; content.value = ''; notice.value = t('geminiCache.success'); resetMutation(); await load() }
  } catch (e) { if (!disposed && !axios.isCancel(e) && ownsPending(pending)) { const kind = cacheErrorKind(e, true); uncertain.value = uncertain.value || kind === 'pending'; if (!uncertain.value) releasePending(pending); formError.value = t(`geminiCache.errors.${uncertain.value ? 'pending' : kind}`); await load() } }
  finally { if (!disposed) busy.value = false }
}
async function loadKeys(page = 1) {
  if (busy.value || uncertain.value) return
  keyController?.abort(); const controller = new AbortController(); keyController = controller
  try {
    const result = await listCacheKeys(page, '', controller.signal)
    if (!disposed && !controller.signal.aborted) { keys.value = result.items; keyPage.value = result.page; keyPages.value = result.pages; keyId.value = keys.value[0]?.id ?? null }
  } catch (e) { if (!disposed && !controller.signal.aborted && !axios.isCancel(e)) error.value = t('geminiCache.errors.keys') }
}
onMounted(() => {
  interval = setInterval(() => { now.value = Date.now() }, 1000)
  const userID = auth.user?.id
  pendingOperation.value = userID ? readPendingCacheOperation(userID) : null
  if (pendingOperation.value) { uncertain.value = true; notice.value = t('geminiCache.pendingHint'); void checkPending() }
  else void loadKeys()
  window.addEventListener('storage', synchronizePending)
})
function synchronizePending() {
  const saved = auth.user ? readPendingCacheOperation(auth.user.id) : null
  if (saved && saved.id !== pendingOperation.value?.id) { pendingOperation.value = saved; pendingNotRecorded.value = false; uncertain.value = true; dialog.value = null; notice.value = t('geminiCache.pendingHint') }
}
onUnmounted(() => { window.removeEventListener('storage', synchronizePending); disposed = true; keyController?.abort(); listController?.abort(); operationController?.abort(); if (interval) clearInterval(interval) })
</script>
