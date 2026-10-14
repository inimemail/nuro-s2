<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import axios from 'axios'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import { useStepUp, isStepUpCancelled } from '@/composables/useStepUp'
import { listReconcile, reconcileCache, type CacheReconcileRow } from '@/api/admin/geminiCache'
import { cacheMoney } from '@/utils/geminiExplicitCache'

const { t } = useI18n()
const stepUp = useStepUp()
const items = ref<CacheReconcileRow[]>([]), nextPage = ref(''), loading = ref(false), busy = ref(false)
const error = ref(''), notice = ref(''), reason = ref(''), target = ref<CacheReconcileRow | null>(null)
const action = ref<'recheck' | 'reserve-deficit'>('recheck')
let controller: AbortController | undefined, disposed = false
async function load(append = false) {
  controller?.abort(); const request = new AbortController(); controller = request
  loading.value = true; error.value = ''
  try {
    const result = await listReconcile(append ? nextPage.value : '', request.signal)
    if (disposed || request.signal.aborted) return
    items.value = append ? [...items.value, ...result.items.filter(row => !items.value.some(old => old.operationId === row.operationId))] : result.items
    nextPage.value = result.nextPageToken
  } catch (e) { if (!disposed && !request.signal.aborted && !axios.isCancel(e)) error.value = t('geminiCache.admin.loadError') }
  finally { if (!disposed && controller === request) loading.value = false }
}
function open(row: CacheReconcileRow, kind: typeof action.value) { target.value = row; action.value = kind; reason.value = ''; error.value = '' }
async function submit() {
  const row = target.value, kind = action.value, explanation = reason.value.trim()
  if (!row || busy.value || explanation.length < 3 || explanation.length > 256) return
  busy.value = true; error.value = ''
  try {
    await stepUp.run(() => reconcileCache(row, kind, explanation))
    if (!disposed) { target.value = null; notice.value = t('geminiCache.admin.queued'); await load() }
  } catch (e) { if (!disposed && !isStepUpCancelled(e)) error.value = t('geminiCache.admin.actionError') }
  finally { if (!disposed) busy.value = false }
}
onMounted(() => void load())
onUnmounted(() => { disposed = true; controller?.abort() })
</script>

<template>
  <AppLayout>
    <div class="mx-auto max-w-7xl space-y-6">
      <section class="rounded-2xl border border-teal-100 bg-gradient-to-br from-teal-50 via-white to-cyan-50 p-6 dark:border-teal-900/40 dark:from-teal-950/30 dark:via-dark-800 dark:to-dark-800 sm:p-8">
        <div class="flex flex-wrap items-center justify-between gap-4">
          <div class="max-w-3xl"><p class="mb-3 text-xs font-semibold tracking-widest text-teal-700 dark:text-teal-300">GEMINI · RECONCILIATION</p><h1 class="text-2xl font-semibold tracking-tight sm:text-3xl">{{ t('geminiCache.admin.title') }}</h1><p class="mt-3 text-sm leading-6 text-gray-600 dark:text-gray-400">{{ t('geminiCache.admin.description') }}</p></div>
          <button class="btn btn-secondary" :disabled="loading || busy" @click="load()"><Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />{{ t('common.refresh') }}</button>
        </div>
      </section>
      <p v-if="notice" role="status" class="rounded-xl border border-teal-200 bg-teal-50 p-4 text-sm text-teal-800 dark:border-teal-900 dark:bg-teal-950/30 dark:text-teal-300">{{ notice }}</p>
      <p v-if="error" role="alert" class="rounded-xl bg-red-50 p-4 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-300">{{ error }}</p>
      <div v-if="loading && !items.length" class="h-48 animate-pulse rounded-2xl bg-gray-100 dark:bg-dark-700" aria-busy="true" />
      <div v-else-if="!items.length && !error" class="rounded-2xl border border-dashed border-gray-300 p-12 text-center dark:border-dark-600"><Icon name="checkCircle" size="xl" class="mx-auto text-teal-500" /><h2 class="mt-4 font-semibold">{{ t('geminiCache.admin.empty') }}</h2><p class="mt-2 text-sm text-gray-500">{{ t('geminiCache.admin.emptyHint') }}</p></div>
      <div v-else class="grid gap-4 lg:grid-cols-2">
        <article v-for="row in items" :key="row.operationId" class="rounded-2xl border border-gray-200 bg-white p-5 shadow-sm dark:border-dark-700 dark:bg-dark-800">
          <div class="flex flex-wrap items-center justify-between gap-2"><span class="rounded-full bg-amber-50 px-3 py-1 text-xs font-medium text-amber-700 dark:bg-amber-950/30 dark:text-amber-300">{{ row.state }}</span><span class="text-xs text-gray-500">{{ row.kind }} · {{ t('geminiCache.admin.attempts', { count: row.attempts }) }}</span></div>
          <p class="mt-4 break-all font-mono text-xs text-gray-600 dark:text-gray-400">{{ row.operationId }}</p><p class="mt-2 break-all font-mono text-xs text-gray-500">{{ row.resourceName }}</p>
          <p class="mt-3 text-xs text-gray-500">User #{{ row.userId }} · Key #{{ row.apiKeyId }} · Group #{{ row.groupId }}</p>
          <div class="mt-4 grid grid-cols-3 gap-3 rounded-xl bg-gray-50 p-3 dark:bg-dark-900/50"><div><p class="text-xs text-gray-500">{{ t('geminiCache.reserved') }}</p><p class="mt-1 break-all font-mono text-sm">{{ cacheMoney(row.reservedAmount) }}</p></div><div><p class="text-xs text-gray-500">{{ t('geminiCache.admin.actual') }}</p><p class="mt-1 break-all font-mono text-sm">{{ row.actualAmount ? cacheMoney(row.actualAmount) : '—' }}</p></div><div><p class="text-xs text-gray-500">{{ t('geminiCache.admin.deficit') }}</p><p class="mt-1 break-all font-mono text-sm text-amber-700 dark:text-amber-300">{{ cacheMoney(row.deficitAmount) }}</p></div></div>
          <p class="mt-3 text-xs text-gray-500">{{ t('geminiCache.tokens') }}: {{ row.confirmedTokens?.toLocaleString() ?? '—' }}<span v-if="row.errorCode" class="mt-2 block break-all font-mono">{{ row.errorCode }}</span></p>
          <div class="mt-4 flex flex-wrap gap-3 border-t border-gray-100 pt-4 dark:border-dark-700"><button class="btn btn-secondary" :disabled="busy || (row.state !== 'reconcile_required' && row.attempts < 12)" @click="open(row, 'recheck')">{{ t('geminiCache.admin.recheck') }}</button><button v-if="Number(row.deficitAmount) > 0 && row.actualAmount" class="btn btn-primary" :disabled="busy" @click="open(row, 'reserve-deficit')">{{ t('geminiCache.admin.reserveDeficit') }}</button></div>
        </article>
      </div>
      <button v-if="nextPage" class="btn btn-secondary mx-auto flex" :disabled="loading || busy" @click="load(true)">{{ t('geminiCache.more') }}</button>
      <p class="text-xs leading-6 text-gray-500">{{ t('geminiCache.admin.policy') }}</p>
    </div>
    <BaseDialog :show="!!target" :title="t(action === 'recheck' ? 'geminiCache.admin.recheck' : 'geminiCache.admin.reserveDeficit')" :close-on-escape="!busy" @close="!busy && (target = null)">
      <form id="cache-reconcile-form" class="space-y-4" @submit.prevent="submit"><p class="text-sm leading-6 text-gray-600 dark:text-gray-400">{{ t(action === 'recheck' ? 'geminiCache.admin.recheckHint' : 'geminiCache.admin.deficitHint', { amount: cacheMoney(target?.deficitAmount), actual: cacheMoney(target?.actualAmount) }) }}</p><p class="break-all font-mono text-xs text-gray-500">{{ target?.operationId }}</p><label class="block"><span class="input-label">{{ t('geminiCache.admin.reason') }}</span><textarea v-model="reason" class="input mt-2" rows="3" minlength="3" maxlength="256" required :disabled="busy" /></label><p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p></form>
      <template #footer><button class="btn btn-secondary" :disabled="busy" @click="target = null">{{ t('common.cancel') }}</button><button class="btn btn-primary" form="cache-reconcile-form" type="submit" :disabled="busy || reason.trim().length < 3">{{ busy ? t('geminiCache.processing') : t('common.confirm') }}</button></template>
    </BaseDialog>
    <TotpStepUpDialog :controller="stepUp" />
  </AppLayout>
</template>
