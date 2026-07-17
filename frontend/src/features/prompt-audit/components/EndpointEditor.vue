<template>
  <div class="border-t border-gray-100 px-5 py-5 first:border-t-0 dark:border-dark-700 sm:px-6">
    <div class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
      <div class="min-w-0">
        <div class="flex min-w-0 items-center gap-2">
          <span class="text-sm font-semibold text-gray-900 dark:text-white">{{ copy.endpoint }} {{ index + 1 }}</span>
          <span
            class="inline-flex rounded px-2 py-0.5 text-xs font-medium"
            :class="endpoint.enabled ? 'bg-green-50 text-green-700 dark:bg-green-900/30 dark:text-green-300' : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'"
          >
            {{ endpoint.enabled ? copy.enabled : copy.disabled }}
          </span>
          <span
            class="inline-flex rounded px-2 py-0.5 text-xs font-medium"
            :class="endpoint.has_token && !endpoint.clear_token ? 'bg-sky-50 text-sky-700 dark:bg-sky-900/30 dark:text-sky-300' : 'bg-gray-100 text-gray-500 dark:bg-dark-700 dark:text-gray-400'"
          >
            {{ endpoint.has_token && !endpoint.clear_token ? copy.tokenConfigured : copy.tokenMissing }}
          </span>
        </div>
        <p v-if="probeResult" class="mt-1 flex items-center gap-1.5 text-xs" :class="probeResult.ok ? 'text-green-700 dark:text-green-300' : 'text-amber-700 dark:text-amber-300'">
          <span class="h-1.5 w-1.5 rounded-full" :class="probeResult.ok ? 'bg-green-500' : 'bg-amber-500'"></span>
          {{ probeResult.ok ? copy.healthy : copy.unhealthy }}
          <span v-if="probeResult.latency_ms >= 0">{{ copy.latency }} {{ probeResult.latency_ms }} ms</span>
        </p>
      </div>

      <div class="flex flex-shrink-0 items-center gap-2">
        <label class="flex items-center gap-2 text-sm text-gray-600 dark:text-gray-300">
          <Toggle v-model="endpoint.enabled" />
          <span>{{ copy.endpointEnabled }}</span>
        </label>
        <button type="button" class="btn btn-secondary btn-sm inline-flex items-center gap-1.5" :disabled="probeLoading || !canProbe" @click="emit('probe')">
          <Icon name="beaker" size="sm" :class="probeLoading ? 'animate-spin' : ''" />
          {{ probeLoading ? copy.probing : copy.probe }}
        </button>
        <button type="button" class="btn btn-ghost btn-sm text-red-600 dark:text-red-400" :title="copy.remove" :disabled="probeLoading" @click="emit('remove')">
          <Icon name="trash" size="sm" />
        </button>
      </div>
    </div>

    <div class="mt-5 grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-4">
      <label class="block">
        <span class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ copy.endpointId }}</span>
        <input v-model.trim="endpoint.id" class="input font-mono" autocomplete="off" />
      </label>
      <label class="block">
        <span class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ copy.endpointName }}</span>
        <input v-model.trim="endpoint.name" class="input" autocomplete="off" />
      </label>
      <label class="block md:col-span-2">
        <span class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ copy.baseUrl }}</span>
        <input v-model.trim="endpoint.base_url" class="input font-mono" inputmode="url" autocomplete="url" placeholder="https://guard.example.com/v1" />
      </label>
      <label class="block md:col-span-2 xl:col-span-1">
        <span class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ copy.model }}</span>
        <input v-model.trim="endpoint.model" class="input font-mono" autocomplete="off" />
      </label>
      <label class="block">
        <span class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ copy.timeout }}</span>
        <input v-model.number="endpoint.timeout_ms" class="input" type="number" min="100" max="30000" step="100" />
      </label>
      <label class="block md:col-span-2">
        <span class="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ copy.token }}</span>
        <input
          v-model="endpoint.token"
          class="input font-mono"
          type="password"
          autocomplete="new-password"
          :disabled="endpoint.clear_token"
          :placeholder="endpoint.has_token ? copy.tokenKeepPlaceholder : copy.tokenNewPlaceholder"
        />
      </label>
    </div>

    <div class="mt-4 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
      <label v-if="endpoint.has_token" class="flex items-center gap-2 text-sm text-gray-600 dark:text-gray-300">
        <input v-model="endpoint.clear_token" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" @change="onClearTokenChange" />
        <span>{{ copy.clearToken }}</span>
      </label>
      <p v-if="endpoint.has_token && !endpoint.token && !endpoint.clear_token" class="text-xs text-gray-500 dark:text-gray-400 sm:ml-auto">
        {{ copy.probeTokenHint }}
      </p>
    </div>

    <div class="mt-5 border-t border-gray-100 pt-4 dark:border-dark-700">
      <label class="flex items-center gap-3 text-sm font-medium text-gray-700 dark:text-gray-300">
        <Toggle v-model="endpoint.allow_private" />
        <span>{{ copy.privateEndpoint }}</span>
      </label>
      <div v-if="endpoint.allow_private" class="mt-3 rounded-md border border-amber-200 bg-amber-50 p-3 dark:border-amber-900/60 dark:bg-amber-900/20">
        <div class="flex items-start gap-2 text-sm text-amber-800 dark:text-amber-200">
          <Icon name="exclamationTriangle" size="sm" class="mt-0.5 flex-shrink-0" />
          <p>{{ copy.privateWarning }}</p>
        </div>
        <label class="mt-3 block">
          <span class="mb-1.5 block text-sm font-medium">{{ copy.cidrs }}</span>
          <textarea v-model="endpoint.allowed_cidrs_text" class="input min-h-24 resize-y font-mono text-sm" :placeholder="copy.cidrsPlaceholder"></textarea>
        </label>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import Toggle from '@/components/common/Toggle.vue'
import Icon from '@/components/icons/Icon.vue'
import type { PromptAuditCopy } from '../copy'
import type { PromptAuditEndpointForm, PromptAuditProbeResult } from '../types'

const endpoint = defineModel<PromptAuditEndpointForm>({ required: true })
defineProps<{
  index: number
  copy: PromptAuditCopy
  probeLoading: boolean
  probeResult?: PromptAuditProbeResult
}>()
const emit = defineEmits<{ (event: 'remove'): void; (event: 'probe'): void }>()

const canProbe = computed(() => Boolean(endpoint.value.base_url.trim() && endpoint.value.model.trim()))

function onClearTokenChange() {
  if (endpoint.value.clear_token) endpoint.value.token = ''
}
</script>
