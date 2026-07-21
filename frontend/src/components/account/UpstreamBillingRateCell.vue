<template>
  <div v-if="eligible" class="flex min-w-[10rem] max-w-[15rem] items-start gap-1.5">
    <div class="min-w-0 flex-1">
      <div data-testid="upstream-billing-rate" class="font-mono text-sm font-semibold text-gray-800 dark:text-gray-100">{{ displayedRate }}</div>
      <div data-testid="upstream-billing-status" :class="statusClass" class="mt-0.5 truncate text-[10px]" :title="statusTitle">{{ statusLabel }}</div>
      <div v-if="protectedGroups.length" class="mt-1.5 flex flex-wrap gap-1">
        <div
          v-for="item in visibleProtectedGroups"
          :key="item.groupId"
          class="group/guard relative"
        >
          <span
            :class="item.badgeClass"
            :data-guard-state="item.state"
            :data-testid="`upstream-billing-guard-group-${item.groupId}`"
            class="inline-flex max-w-[7rem] items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-medium leading-4"
          >
            <span :class="item.dotClass" class="h-1.5 w-1.5 flex-none rounded-full" />
            <span class="truncate">{{ item.name }}</span>
          </span>
          <div :data-testid="`upstream-billing-guard-group-${item.groupId}-details`" class="pointer-events-none absolute bottom-full left-0 z-[120] mb-1.5 w-64 rounded bg-gray-900 px-3 py-2 text-xs leading-5 text-white opacity-0 shadow-lg transition-opacity group-hover/guard:opacity-100 dark:bg-dark-700">
            <div class="font-medium">{{ item.name }}</div>
            <div class="mt-1 text-gray-300">{{ t('admin.accounts.upstreamBilling.currentRateDetail', { rate: displayedRate }) }}</div>
            <div class="text-gray-300">{{ t('admin.accounts.upstreamBilling.guardLimitDetail', { rate: `${item.limit}x` }) }}</div>
            <div class="text-gray-400">{{ statusTitle }}</div>
            <div :class="item.detailClass">{{ item.detail }}</div>
            <div class="absolute left-3 top-full border-4 border-transparent border-t-gray-900 dark:border-t-gray-700" />
          </div>
        </div>
        <div v-if="hiddenProtectedGroupCount" class="group/guard-more relative">
          <span class="inline-flex rounded bg-gray-100 px-1.5 py-0.5 text-[10px] leading-4 text-gray-500 dark:bg-dark-600 dark:text-gray-400">
            +{{ hiddenProtectedGroupCount }}
          </span>
          <div data-testid="upstream-billing-hidden-group-details" class="pointer-events-none absolute bottom-full right-0 z-[120] mb-1.5 w-64 rounded bg-gray-900 px-3 py-2 text-xs leading-5 text-white opacity-0 shadow-lg transition-opacity group-hover/guard-more:opacity-100 dark:bg-dark-700">
            <div class="text-gray-300">{{ t('admin.accounts.upstreamBilling.currentRateDetail', { rate: displayedRate }) }}</div>
            <div class="mb-1 text-gray-400">{{ statusTitle }}</div>
            <div v-for="item in hiddenProtectedGroups" :key="item.groupId" class="border-t border-gray-700 py-1 first:border-t-0 dark:border-dark-600">
              <div class="flex items-center justify-between gap-3">
                <span class="truncate">{{ item.name }}</span>
                <span class="flex-none text-gray-300">{{ item.limit }}x</span>
              </div>
              <div :class="item.detailClass" class="truncate">{{ item.detail }}</div>
            </div>
            <div class="absolute right-3 top-full border-4 border-transparent border-t-gray-900 dark:border-t-gray-700" />
          </div>
        </div>
      </div>
    </div>
    <button
      type="button"
      class="inline-flex h-7 w-7 flex-shrink-0 items-center justify-center rounded text-primary-600 transition-colors hover:bg-primary-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-primary-400 dark:hover:bg-primary-900/30"
      :disabled="probing"
      :aria-label="t('admin.accounts.upstreamBilling.manualProbe')"
      :title="t('admin.accounts.upstreamBilling.manualProbe')"
      @click="$emit('probe')"
    >
      <Icon name="refresh" size="xs" :class="{ 'animate-spin': probing }" />
    </button>
  </div>
  <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import type { Account, UpstreamBillingProbeSnapshot } from '@/types'

const props = defineProps<{
  account: Account
  now: number
  globalProbeEnabled?: boolean
  probing?: boolean
}>()

defineEmits<{
  (event: 'probe'): void
}>()

const { t } = useI18n()
const eligible = computed(() => props.account.platform === 'openai' && props.account.type === 'apikey')
const snapshot = computed<UpstreamBillingProbeSnapshot | undefined>(
  () => props.account.extra?.upstream_billing_probe
)
const freshUntil = computed(() => {
  const value = snapshot.value?.fresh_until
  return typeof value === 'string' ? Date.parse(value) : Number.NaN
})
const stale = computed(() => {
  if (!snapshot.value) return false
  return !Number.isFinite(freshUntil.value) || props.now >= freshUntil.value
})
const displayedRate = computed(() => {
  const value = snapshot.value?.data?.effective_rate_multiplier
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
    ? `${Number(value.toPrecision(12))}x`
    : '-'
})
const observedRate = computed(() => {
  const value = snapshot.value?.data?.effective_rate_multiplier
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
})
const guardObservedRate = computed(() => {
  const value = props.account.upstream_billing_guard_observed_multiplier
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
})
const autoProbeEnabled = computed(() => props.account.extra?.upstream_billing_probe_enabled === true)
const globalProbeDisabled = computed(() => props.globalProbeEnabled === false)
const protectedGroups = computed(() => {
  const groups = new Map((props.account.groups || []).map((group) => [group.id, group]))
  return (props.account.account_groups || [])
    .map((binding) => {
      const mappedGroup = groups.get(binding.group_id)
      const group = mappedGroup || binding.group
      const policyGroup = mappedGroup && Object.prototype.hasOwnProperty.call(mappedGroup, 'upstream_billing_guard_max_multiplier')
        ? mappedGroup
        : binding.group && Object.prototype.hasOwnProperty.call(binding.group, 'upstream_billing_guard_max_multiplier')
          ? binding.group
          : undefined
      const hasRawOverride = Object.prototype.hasOwnProperty.call(binding, 'upstream_billing_guard_override_max_multiplier')
      const rawOverride = hasRawOverride
        ? binding.upstream_billing_guard_override_max_multiplier
        : binding.upstream_billing_guard_max_multiplier
      const limit = policyGroup
        ? (policyGroup.upstream_billing_guard_max_multiplier == null
          ? null
          : rawOverride == null
            ? policyGroup.upstream_billing_guard_max_multiplier
            : Math.min(rawOverride, policyGroup.upstream_billing_guard_max_multiplier))
        : binding.upstream_billing_guard_max_multiplier
      return { binding, group, limit }
    })
    .filter((item) => typeof item.limit === 'number' && Number.isFinite(item.limit) && item.limit >= 0)
    .map((binding) => {
      const limit = binding.limit as number
      const disabled = props.account.upstream_billing_guard_enabled !== true
      const blocked = !disabled && (
        !autoProbeEnabled.value || (guardObservedRate.value != null && guardObservedRate.value > limit)
      )
      const pending = !disabled && !blocked && (
        globalProbeDisabled.value || guardObservedRate.value == null
      )
      const state = disabled ? 'disabled' : blocked ? 'blocked' : pending ? 'pending' : 'available'
      return {
        groupId: binding.binding.group_id,
        name: binding.group?.name || `#${binding.binding.group_id}`,
        limit: Number(limit.toPrecision(8)),
        blocked,
        state,
        badgeClass: blocked
          ? 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
          : pending || disabled
            ? 'bg-gray-100 text-gray-600 dark:bg-dark-600 dark:text-gray-300'
            : 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300',
        dotClass: blocked ? 'bg-amber-500' : pending || disabled ? 'bg-gray-400' : 'bg-emerald-500',
        detailClass: blocked ? 'text-amber-300' : pending || disabled ? 'text-gray-300' : 'text-emerald-300',
        detail: disabled
          ? t('admin.accounts.upstreamBilling.guardDisabled')
          : blocked
            ? !autoProbeEnabled.value
              ? t('admin.accounts.upstreamBilling.guardProbeDisabled')
              : t('admin.accounts.upstreamBilling.guardPaused')
            : globalProbeDisabled.value
              ? t('admin.accounts.upstreamBilling.guardGlobalProbeDisabled')
          : !autoProbeEnabled.value
          ? t('admin.accounts.upstreamBilling.guardProbeDisabled')
          : pending
            ? t('admin.accounts.upstreamBilling.guardWaitingFirstProbe')
              : t('admin.accounts.upstreamBilling.guardAvailable')
      }
    })
})
const visibleProtectedGroups = computed(() => protectedGroups.value.slice(0, 3))
const hiddenProtectedGroups = computed(() => protectedGroups.value.slice(3))
const hiddenProtectedGroupCount = computed(() => Math.max(0, protectedGroups.value.length - visibleProtectedGroups.value.length))
const receivedAgeLabel = computed(() => {
  const value = snapshot.value?.received_at
  if (!value) return null
  const timestamp = Date.parse(value)
  if (!Number.isFinite(timestamp)) return null
  const seconds = Math.max(0, Math.floor((props.now - timestamp) / 1000))
  if (seconds < 60) return t('common.time.justNow')
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return t('common.time.minutesAgo', { n: minutes })
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return t('common.time.hoursAgo', { n: hours })
  return t('common.time.daysAgo', { n: Math.floor(hours / 24) })
})
const statusLabel = computed(() => {
  if (!autoProbeEnabled.value) return t('admin.accounts.upstreamBilling.autoProbeDisabled')
  if (props.globalProbeEnabled === false) return t('admin.accounts.upstreamBilling.globalProbeDisabled')
  if (!snapshot.value) return t('admin.accounts.upstreamBilling.notProbed')
  if (snapshot.value.status === 'unsupported') return t('admin.accounts.upstreamBilling.unsupported')
  if (snapshot.value.status === 'failed') {
    return observedRate.value == null
      ? t('admin.accounts.upstreamBilling.failed')
      : t('admin.accounts.upstreamBilling.failedWithLast')
  }
  return receivedAgeLabel.value == null
    ? t('admin.accounts.upstreamBilling.observed')
    : receivedAgeLabel.value
})
const statusClass = computed(() => {
  if (!autoProbeEnabled.value || props.globalProbeEnabled === false) return 'text-gray-500 dark:text-gray-400'
  if (snapshot.value?.status === 'failed') return 'text-red-600 dark:text-red-400'
  if (snapshot.value?.status === 'unsupported') return 'text-gray-500 dark:text-gray-400'
  if (stale.value) return 'text-amber-600 dark:text-amber-400'
  if (snapshot.value?.status === 'ok') return 'text-emerald-600 dark:text-emerald-400'
  return 'text-gray-400 dark:text-gray-500'
})
const statusTitle = computed(() => {
  if (!autoProbeEnabled.value || props.globalProbeEnabled === false) return statusLabel.value
  const value = snapshot.value?.received_at || snapshot.value?.last_attempt_at
  if (!value) return statusLabel.value
  const timestamp = Date.parse(value)
  if (!Number.isFinite(timestamp)) return statusLabel.value
  return `${statusLabel.value} · ${new Date(timestamp).toLocaleString()}`
})
</script>
