<template>
  <div class="flex items-start gap-2 rounded border p-2"
       :class="isEmpty ? 'border-red-400 bg-red-50 dark:border-red-500 dark:bg-red-950/20' : 'border-gray-200 bg-white dark:border-dark-500 dark:bg-dark-700'">
    <!-- Token mode: context range + prices ($/MTok) -->
    <template v-if="mode === 'token'">
      <div class="grid min-w-0 flex-1 grid-cols-2 gap-2 sm:grid-cols-4 xl:grid-cols-6">
        <div>
          <label class="text-xs text-gray-400">Min</label>
          <input :value="interval.min_tokens" @input="emitField('min_tokens', toInt(($event.target as HTMLInputElement).value))"
            type="number" min="0" class="input mt-0.5 text-xs" />
        </div>
        <div>
          <label class="text-xs text-gray-400">Max <span class="text-gray-300">(含)</span></label>
          <input :value="interval.max_tokens ?? ''" @input="emitField('max_tokens', toIntOrNull(($event.target as HTMLInputElement).value))"
            type="number" min="0" class="input mt-0.5 text-xs" :placeholder="'∞'" />
        </div>
        <div class="col-span-2 flex items-end sm:col-span-2 xl:col-span-1">
          <div class="grid h-9 w-full grid-cols-2 rounded border border-gray-200 bg-gray-50 p-0.5 dark:border-dark-500 dark:bg-dark-800">
            <button type="button" class="rounded text-xs font-medium transition-colors" :class="!usesMultipliers ? 'bg-white text-primary-600 shadow-sm dark:bg-dark-600 dark:text-primary-300' : 'text-gray-400'" @click="setValueMode('price')">$/M</button>
            <button type="button" class="rounded text-xs font-medium transition-colors" :class="usesMultipliers ? 'bg-white text-primary-600 shadow-sm dark:bg-dark-600 dark:text-primary-300' : 'text-gray-400'" @click="setValueMode('multiplier')">×</button>
          </div>
        </div>
        <template v-if="!usesMultipliers">
          <div v-for="field in priceFields" :key="field.key">
            <label class="text-xs text-gray-400">{{ field.label }} <span class="text-gray-300">$/M</span></label>
            <input :value="interval[field.key]" @input="emitField(field.key, ($event.target as HTMLInputElement).value)" type="number" step="any" min="0" class="input mt-0.5 text-xs" />
          </div>
        </template>
        <template v-else>
          <div v-for="field in multiplierFields" :key="field.key">
            <label class="text-xs text-gray-400">{{ field.label }} <span class="text-gray-300">×</span></label>
            <input :value="interval[field.key]" @input="emitField(field.key, ($event.target as HTMLInputElement).value)" type="number" step="any" min="0.000001" class="input mt-0.5 text-xs" placeholder="1" />
          </div>
        </template>
      </div>
    </template>

    <!-- Per-request / Image mode: tier label + context range + price -->
    <template v-else>
      <div class="w-24">
        <label class="text-xs text-gray-400">
          {{ mode === 'image' || mode === 'video' ? t('admin.channels.form.resolution', '分辨率') : t('admin.channels.form.tierLabel', '层级') }}
        </label>
        <input :value="interval.tier_label" @input="emitField('tier_label', ($event.target as HTMLInputElement).value)"
          type="text" class="input mt-0.5 text-xs" :placeholder="mode === 'video' ? '480p / 720p / 1080p' : mode === 'image' ? '1K / 2K / 4K' : ''" />
      </div>
      <div class="w-20">
        <label class="text-xs text-gray-400">Min</label>
        <input :value="interval.min_tokens" @input="emitField('min_tokens', toInt(($event.target as HTMLInputElement).value))"
          type="number" min="0" class="input mt-0.5 text-xs" />
      </div>
      <div class="w-20">
        <label class="text-xs text-gray-400">Max <span class="text-gray-300">(含)</span></label>
        <input :value="interval.max_tokens ?? ''" @input="emitField('max_tokens', toIntOrNull(($event.target as HTMLInputElement).value))"
          type="number" min="0" class="input mt-0.5 text-xs" :placeholder="'∞'" />
      </div>
      <div class="flex-1">
        <label class="text-xs text-gray-400">{{ mode === 'video' ? t('admin.groups.modelPricing.perSecondPrice') : t('admin.channels.form.perRequestPrice', '单次价格') }} <span v-if="isEmpty" class="text-red-500">*</span> <span class="text-gray-300">{{ mode === 'video' ? '$/s' : '$' }}</span></label>
        <input :value="interval.per_request_price" @input="emitField('per_request_price', ($event.target as HTMLInputElement).value)"
          type="number" step="any" min="0" class="input mt-0.5 text-xs" />
      </div>
    </template>

    <button type="button" @click="emit('remove')" class="mt-4 rounded p-0.5 text-gray-400 hover:text-red-500">
      <Icon name="x" size="sm" />
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import type { IntervalFormEntry } from './types'
import type { BillingMode } from '@/api/admin/channels'

const { t } = useI18n()

const props = defineProps<{
  interval: IntervalFormEntry
  mode: BillingMode
  enableMultipliers?: boolean
}>()

const emit = defineEmits<{
  update: [interval: IntervalFormEntry]
  remove: []
}>()

// 检测所有价格字段是否都为空
const isEmpty = computed(() => {
  const iv = props.interval
  return (iv.input_price == null || iv.input_price === '') &&
    (iv.output_price == null || iv.output_price === '') &&
    (iv.cache_write_price == null || iv.cache_write_price === '') &&
    (iv.cache_write_1h_price == null || iv.cache_write_1h_price === '') &&
    (iv.cache_read_price == null || iv.cache_read_price === '') &&
    (iv.input_multiplier == null || iv.input_multiplier === '') &&
    (iv.output_multiplier == null || iv.output_multiplier === '') &&
    (iv.cache_write_multiplier == null || iv.cache_write_multiplier === '') &&
    (iv.cache_read_multiplier == null || iv.cache_read_multiplier === '') &&
    (iv.per_request_price == null || iv.per_request_price === '')
})

const usesMultipliers = computed(() => props.enableMultipliers !== false && [
  props.interval.input_multiplier,
  props.interval.output_multiplier,
  props.interval.cache_write_multiplier,
  props.interval.cache_read_multiplier
].some(value => value != null && value !== ''))

const priceFields: Array<{ key: keyof IntervalFormEntry; label: string }> = [
  { key: 'input_price', label: t('admin.channels.form.inputPrice', '输入') },
  { key: 'output_price', label: t('admin.channels.form.outputPrice', '输出') },
  { key: 'cache_write_price', label: t('admin.channels.form.cacheWritePrice', '缓存写入') },
  { key: 'cache_write_1h_price', label: t('admin.channels.form.cacheWrite1hPrice', '1h 缓存写入') },
  { key: 'cache_read_price', label: t('admin.channels.form.cacheReadPrice', '缓存读取') }
]

const multiplierFields: Array<{ key: keyof IntervalFormEntry; label: string }> = [
  { key: 'input_multiplier', label: t('admin.channels.form.inputMultiplier', '输入') },
  { key: 'output_multiplier', label: t('admin.channels.form.outputMultiplier', '输出') },
  { key: 'cache_write_multiplier', label: t('admin.channels.form.cacheWriteMultiplier', '缓存写入') },
  { key: 'cache_read_multiplier', label: t('admin.channels.form.cacheReadMultiplier', '缓存读取') }
]

function setValueMode(mode: 'price' | 'multiplier') {
  const next = { ...props.interval }
  if (mode === 'price') {
    next.input_multiplier = null
    next.output_multiplier = null
    next.cache_write_multiplier = null
    next.cache_read_multiplier = null
  } else {
    next.input_price = null
    next.output_price = null
    next.cache_write_price = null
    next.cache_write_1h_price = null
    next.cache_read_price = null
  }
  emit('update', next)
}

function emitField(field: keyof IntervalFormEntry, value: string | number | null) {
  emit('update', { ...props.interval, [field]: value === '' ? null : value })
}

function toInt(val: string): number {
  const n = parseInt(val, 10)
  return isNaN(n) ? 0 : n
}

function toIntOrNull(val: string): number | null {
  if (val === '') return null
  const n = parseInt(val, 10)
  return isNaN(n) ? null : n
}
</script>
