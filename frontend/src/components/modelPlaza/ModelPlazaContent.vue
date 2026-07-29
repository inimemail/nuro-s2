<template>
  <div class="mx-auto w-full max-w-[1500px] space-y-5">
    <header v-if="!embedded" class="border-b border-gray-200 pb-5 dark:border-dark-700">
      <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">{{ t('modelPlaza.title') }}</h1>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('modelPlaza.description') }}</p>
    </header>

    <div v-if="descriptionHtml" class="border-l-4 border-primary-500 bg-white px-5 py-4 text-sm text-gray-700 shadow-sm dark:bg-dark-800 dark:text-gray-200" v-html="descriptionHtml"></div>

    <div v-if="loading" class="flex min-h-[280px] items-center justify-center">
      <span class="h-8 w-8 animate-spin rounded-full border-2 border-primary-200 border-t-primary-600"></span>
    </div>
    <div v-else-if="error" class="border border-red-200 bg-red-50 px-5 py-10 text-center text-sm text-red-700 dark:border-red-900 dark:bg-red-950/20 dark:text-red-300">
      <p>{{ t('modelPlaza.loadFailed') }}</p>
      <button type="button" class="btn btn-secondary btn-sm mt-4" @click="$emit('retry')">{{ t('common.retry') }}</button>
    </div>
    <template v-else>
      <p v-if="!authenticated" class="flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
        <Icon name="infoCircle" size="xs" />{{ t('modelPlaza.anonymousHint') }}
      </p>

      <div class="sticky top-16 z-20 border-y border-gray-200 bg-gray-50/95 py-3 backdrop-blur dark:border-dark-700 dark:bg-dark-950/95">
        <div class="grid gap-2 md:grid-cols-[minmax(210px,1fr)_repeat(3,minmax(140px,auto))]">
          <label class="relative block">
            <span class="sr-only">{{ t('modelPlaza.filters.modelLabel') }}</span>
            <Icon name="search" size="sm" class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-400" />
            <input v-model="search" class="input h-10 pl-9" :placeholder="t('modelPlaza.filters.searchPlaceholder')" />
          </label>
          <select v-model="platform" class="input h-10">
            <option value="all">{{ t('modelPlaza.filters.allPlatforms') }}</option>
            <option v-for="item in platforms" :key="item" :value="item">{{ item }}</option>
          </select>
          <select v-model="groupId" class="input h-10">
            <option value="all">{{ t('modelPlaza.filters.allGroups') }}</option>
            <option v-for="item in groupOptions" :key="item.id" :value="String(item.id)">{{ item.name }}</option>
          </select>
          <select v-model="availability" class="input h-10">
            <option value="all">{{ t('modelPlaza.filters.allAvailability') }}</option>
            <option value="standard">{{ t('modelPlaza.filters.standard') }}</option>
            <option value="subscription">{{ t('modelPlaza.filters.subscription') }}</option>
            <option value="exclusive">{{ t('modelPlaza.filters.exclusive') }}</option>
          </select>
        </div>
      </div>

      <div v-if="filteredGroups.length" class="divide-y divide-gray-200 border-y border-gray-200 bg-white dark:divide-dark-700 dark:border-dark-700 dark:bg-dark-900">
        <section v-for="group in filteredGroups" :key="group.id" class="scroll-mt-32">
          <header class="flex flex-wrap items-start justify-between gap-3 px-4 py-4 sm:px-5">
            <div class="min-w-0">
              <div class="flex flex-wrap items-center gap-2">
                <h2 class="font-semibold text-gray-900 dark:text-white">{{ group.name }}</h2>
                <span class="rounded border px-2 py-0.5 text-xs font-medium" :class="platformClass(group.platform)">{{ group.platform }}</span>
                <span v-if="group.subscription_type === 'subscription'" class="rounded bg-blue-50 px-2 py-0.5 text-xs text-blue-700 dark:bg-blue-950/30 dark:text-blue-300">{{ t('modelPlaza.badges.subscription') }}</span>
                <span v-if="group.is_exclusive" class="rounded bg-violet-50 px-2 py-0.5 text-xs text-violet-700 dark:bg-violet-950/30 dark:text-violet-300">{{ t('modelPlaza.badges.exclusive') }}</span>
              </div>
              <p v-if="group.description" class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ group.description }}</p>
            </div>
            <div class="shrink-0 text-right text-xs text-gray-500 dark:text-gray-400">
              <div>{{ t('modelPlaza.table.rate') }} <span class="font-mono font-semibold text-gray-900 dark:text-white">{{ effectiveRate(group) }}x</span></div>
              <div v-if="group.peak_rate_enabled" class="mt-1 text-amber-600 dark:text-amber-400">{{ group.peak_start }}-{{ group.peak_end }} / {{ group.peak_rate_multiplier }}x</div>
            </div>
          </header>

          <div class="overflow-x-auto border-t border-gray-100 dark:border-dark-700">
            <table class="w-full min-w-[980px] table-fixed text-sm tabular-nums">
              <colgroup><col class="w-[25%]" /><col class="w-[12%]" /><col class="w-[12%]" /><col class="w-[13%]" /><col class="w-[12%]" /><col class="w-[12%]" /><col class="w-[14%]" /></colgroup>
              <thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-800 dark:text-gray-400 md:sticky md:top-32 md:z-10">
                <tr>
                  <th class="px-4 py-3 text-left font-medium">{{ t('modelPlaza.table.model') }}</th>
                  <th class="px-3 py-3 text-right font-medium">{{ t('modelPlaza.table.input') }}</th>
                  <th class="px-3 py-3 text-right font-medium">{{ t('modelPlaza.table.output') }}</th>
                  <th class="px-3 py-3 text-right font-medium">{{ t('modelPlaza.table.cache') }}</th>
                  <th class="border-l border-gray-200 px-3 py-3 text-right font-medium dark:border-dark-700">{{ t('modelPlaza.table.officialInput') }}</th>
                  <th class="px-3 py-3 text-right font-medium">{{ t('modelPlaza.table.officialOutput') }}</th>
                  <th class="px-3 py-3 text-right font-medium">{{ t('modelPlaza.table.officialCache') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-800">
                <tr v-for="model in group.models" :key="model.name" class="hover:bg-gray-50 dark:hover:bg-dark-800/60">
                  <td class="px-4 py-3">
                    <div class="flex flex-wrap items-center gap-2">
                      <span class="min-w-0 truncate font-medium text-gray-900 dark:text-white">{{ model.name }}</span>
                      <span v-if="billingMode(model) !== BILLING_MODE_TOKEN" class="rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700 dark:text-gray-300">
                        {{ billingModeLabel(model) }}
                      </span>
                      <button type="button" class="shrink-0 rounded p-1 text-gray-400 hover:bg-gray-200 hover:text-primary-600 dark:hover:bg-dark-700" :title="t('modelPlaza.copyModel')" @click="copyModel(model.name)"><Icon name="copy" size="xs" /></button>
                    </div>
                  </td>
                  <template v-if="billingMode(model) === BILLING_MODE_TOKEN">
                    <td class="px-3 py-3 text-right font-mono">
                      <template v-if="tokenIntervals(model).length">
                        <div v-for="(interval, index) in tokenIntervals(model)" :key="index" class="whitespace-nowrap text-xs leading-5">
                          <span class="mr-1 font-sans text-gray-400">{{ tierLabel(interval) }}</span>{{ paid(interval.input_price, group) }}
                        </div>
                      </template>
                      <template v-else>{{ paid(model.pricing?.input_price, group) }}</template>
                    </td>
                    <td class="px-3 py-3 text-right font-mono">
                      <template v-if="tokenIntervals(model).length">
                        <div v-for="(interval, index) in tokenIntervals(model)" :key="index" class="whitespace-nowrap text-xs leading-5">
                          <span class="mr-1 font-sans text-gray-400">{{ tierLabel(interval) }}</span>{{ paid(interval.output_price, group) }}
                        </div>
                      </template>
                      <template v-else>{{ paid(model.pricing?.output_price, group) }}</template>
                    </td>
                    <td class="px-3 py-3 text-right font-mono text-xs">
                      <template v-if="hasCachePricing(model)">
                        <div>{{ cacheLine(t('modelPlaza.table.cacheWrite'), model.pricing?.cache_write_price, group) }}</div>
                        <div>{{ cacheLine(t('modelPlaza.table.cacheRead'), model.pricing?.cache_read_price, group) }}</div>
                      </template>
                      <span v-else>-</span>
                    </td>
                  </template>
                  <td v-else colspan="3" class="px-3 py-3 text-right">
                    <div v-if="requestIntervals(model).length" class="flex flex-wrap justify-end gap-1.5">
                      <span v-for="(interval, index) in requestIntervals(model)" :key="index" class="rounded bg-gray-100 px-2 py-1 font-mono text-xs dark:bg-dark-700">
                        <span class="mr-1 font-sans text-gray-400">{{ tierLabel(interval) }}</span>{{ paidRequest(interval.per_request_price, group) }} {{ billingUnit(model) }}
                      </span>
                    </div>
                    <template v-else>
                      <span class="font-mono font-medium">{{ paidRequest(model.pricing?.per_request_price, group) }}</span>
                      <span v-if="model.pricing?.per_request_price != null" class="ml-1 text-xs text-gray-400">{{ billingUnit(model) }}</span>
                    </template>
                  </td>
                  <td class="border-l border-gray-100 px-3 py-3 text-right font-mono text-gray-500 dark:border-dark-800 dark:text-gray-400">{{ official(model.official_pricing?.input_price) }}</td>
                  <td class="px-3 py-3 text-right font-mono text-gray-500 dark:text-gray-400">{{ official(model.official_pricing?.output_price) }}</td>
                  <td class="px-3 py-3 text-right font-mono text-xs text-gray-500 dark:text-gray-400">
                    <template v-if="hasOfficialCachePricing(model)">
                      <div>
                        {{ cacheLine(t('modelPlaza.table.cacheWrite'), model.official_pricing?.cache_write_price) }}
                        <span v-if="model.official_pricing?.cache_write_1h_price != null" class="block text-[11px] text-gray-400">1h {{ official(model.official_pricing.cache_write_1h_price) }}</span>
                      </div>
                      <div>{{ cacheLine(t('modelPlaza.table.cacheRead'), model.official_pricing?.cache_read_price) }}</div>
                    </template>
                    <span v-else>-</span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      </div>
      <div v-else class="border border-dashed border-gray-300 px-5 py-12 text-center text-sm text-gray-500 dark:border-dark-700 dark:text-gray-400">
        {{ search.trim() ? t('modelPlaza.noSearchResult') : t('modelPlaza.empty') }}
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import Icon from '@/components/icons/Icon.vue'
import { useClipboard } from '@/composables/useClipboard'
import { formatScaled } from '@/utils/pricing'
import type { ModelPlazaGroup, ModelPlazaResponse, PlazaModel } from '@/api/modelPlaza'
import type { UserPricingInterval } from '@/api/channels'
import { BILLING_MODE_IMAGE, BILLING_MODE_TOKEN, type BillingMode } from '@/constants/channel'

const props = defineProps<{ response: ModelPlazaResponse | null; loading: boolean; error: boolean; embedded?: boolean; authenticated: boolean }>()
defineEmits<{ retry: [] }>()
const { t } = useI18n()
const { copyToClipboard } = useClipboard()
const platform = ref('all')
const groupId = ref('all')
const availability = ref('all')
const search = ref('')
const descriptionHtml = computed(() => {
  const source = props.response?.description?.trim()
  return source ? DOMPurify.sanitize(marked.parse(source) as string) : ''
})
const platforms = computed(() => [...new Set((props.response?.groups ?? []).map(group => group.platform).filter(Boolean))].sort())
const groupOptions = computed(() => (props.response?.groups ?? []).filter(group => platform.value === 'all' || group.platform === platform.value))
watch(platform, () => { groupId.value = 'all' })
const filteredGroups = computed(() => {
  const query = search.value.trim().toLowerCase()
  return (props.response?.groups ?? [])
    .filter(group => platform.value === 'all' || group.platform === platform.value)
    .filter(group => groupId.value === 'all' || String(group.id) === groupId.value)
    .filter(group => availability.value === 'all' || (availability.value === 'exclusive' ? group.is_exclusive : group.subscription_type === availability.value && !group.is_exclusive))
    .map(group => ({
      ...group,
      models: group.models
        .filter(model => !query || model.name.toLowerCase().includes(query))
        .sort((a, b) => {
          const aPrice = a.official_pricing?.output_price
          const bPrice = b.official_pricing?.output_price
          if (aPrice != null && bPrice != null && aPrice !== bPrice) return bPrice - aPrice
          if (aPrice != null) return -1
          if (bPrice != null) return 1
          return a.name.localeCompare(b.name)
        })
    }))
    .filter(group => group.models.length > 0)
    .sort((a, b) => effectiveRate(a) - effectiveRate(b) || a.name.localeCompare(b.name))
})
function effectiveRate(group: ModelPlazaGroup): number { return group.user_rate_multiplier ?? group.rate_multiplier }
function money(value: number | null | undefined): string { return formatScaled(value ?? null, 1_000_000, 2) }
function paid(value: number | null | undefined, group: ModelPlazaGroup): string { return value == null ? '-' : money(value * effectiveRate(group)) }
function official(value: number | null | undefined): string { return money(value) }
function cacheLine(label: string, value: number | null | undefined, group?: ModelPlazaGroup): string { return value == null ? '-' : `${label} ${group ? paid(value, group) : official(value)}` }
function billingMode(model: PlazaModel): BillingMode { return model.pricing?.billing_mode || BILLING_MODE_TOKEN }
function billingModeLabel(model: PlazaModel): string { return billingMode(model) === BILLING_MODE_IMAGE ? t('modelPlaza.table.perImage') : t('modelPlaza.table.perRequest') }
function billingUnit(model: PlazaModel): string { return billingMode(model) === BILLING_MODE_IMAGE ? t('modelPlaza.table.perUnitImage') : t('modelPlaza.table.perUnitRequest') }
function paidRequest(value: number | null | undefined, group: ModelPlazaGroup): string {
  if (value == null) return '-'
  return formatScaled(value * effectiveRate(group), 1, 2)
}
function tokenIntervals(model: PlazaModel): UserPricingInterval[] { return model.pricing?.intervals ?? [] }
function requestIntervals(model: PlazaModel): UserPricingInterval[] { return (model.pricing?.intervals ?? []).filter(interval => interval.per_request_price != null) }
function hasCachePricing(model: PlazaModel): boolean { return model.pricing?.cache_write_price != null || model.pricing?.cache_read_price != null }
function hasOfficialCachePricing(model: PlazaModel): boolean {
  const pricing = model.official_pricing
  return pricing?.cache_write_price != null || pricing?.cache_write_1h_price != null || pricing?.cache_read_price != null
}
function tierLabel(interval: UserPricingInterval): string {
  if (interval.tier_label) return interval.tier_label
  if (interval.max_tokens == null) return `>${formatTokenCount(interval.min_tokens)}`
  if (interval.min_tokens === 0) return `≤${formatTokenCount(interval.max_tokens)}`
  return `${formatTokenCount(interval.min_tokens)}-${formatTokenCount(interval.max_tokens)}`
}
function formatTokenCount(value: number): string {
  if (value >= 1_000_000) return `${Number((value / 1_000_000).toFixed(2))}M`
  if (value >= 1_000) return `${Number((value / 1_000).toFixed(2))}K`
  return String(value)
}
function platformClass(value: string): string { return ({ openai: 'border-emerald-200 bg-emerald-50 text-emerald-700 dark:border-emerald-900 dark:bg-emerald-950/30 dark:text-emerald-300', anthropic: 'border-orange-200 bg-orange-50 text-orange-700 dark:border-orange-900 dark:bg-orange-950/30 dark:text-orange-300', gemini: 'border-blue-200 bg-blue-50 text-blue-700 dark:border-blue-900 dark:bg-blue-950/30 dark:text-blue-300', grok: 'border-cyan-200 bg-cyan-50 text-cyan-700 dark:border-cyan-900 dark:bg-cyan-950/30 dark:text-cyan-300' } as Record<string, string>)[value] || 'border-gray-200 bg-gray-50 text-gray-700 dark:border-dark-700 dark:bg-dark-800 dark:text-gray-300' }
async function copyModel(value: string): Promise<void> { await copyToClipboard(value) }
</script>
