<template>
  <div v-if="rows.length > 0" class="space-y-2">
    <div
      v-for="(row, index) in rows"
      :key="getRowKey(row)"
      class="flex items-center gap-2"
    >
      <input
        v-model="row.name"
        type="text"
        class="input flex-1"
        :placeholder="t('admin.accounts.headerOverride.namePlaceholder')"
      />
      <input
        v-model="row.value"
        type="text"
        class="input flex-1"
        :placeholder="t('admin.accounts.headerOverride.valuePlaceholder')"
      />
      <button
        type="button"
        class="rounded-lg p-2 text-red-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20"
        :title="t('common.delete')"
        @click="removeRow(index)"
      >
        <Icon name="trash" size="sm" />
      </button>
    </div>
  </div>

  <button
    type="button"
    class="w-full rounded-lg border-2 border-dashed border-gray-300 px-4 py-2 text-gray-600 transition-colors hover:border-gray-400 hover:text-gray-700 dark:border-dark-500 dark:text-gray-400 dark:hover:border-dark-400 dark:hover:text-gray-300"
    @click="addRow"
  >
    <Icon name="plus" size="sm" class="mr-1 inline" />
    {{ t('admin.accounts.headerOverride.addRow') }}
  </button>

  <button
    type="button"
    class="rounded-lg bg-primary-50 px-3 py-1 text-xs text-primary-700 transition-colors hover:bg-primary-100 dark:bg-primary-900/30 dark:text-primary-400 dark:hover:bg-primary-900/50"
    @click="fillTemplate"
  >
    {{ t('admin.accounts.headerOverride.fillTemplate') }}
  </button>

  <p class="text-xs text-gray-500 dark:text-gray-400">
    {{ t('admin.accounts.headerOverride.emptyValueHint') }}
  </p>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { createStableObjectKeyResolver } from '@/utils/stableObjectKey'
import { getHeaderOverrideTemplate, type HeaderOverrideRow } from './credentialsBuilder'

const props = defineProps<{ rows: HeaderOverrideRow[]; platform: string }>()
const emit = defineEmits<{ (event: 'update:rows', rows: HeaderOverrideRow[]): void }>()
const { t } = useI18n()
const getRowKey = createStableObjectKeyResolver<HeaderOverrideRow>('header-override-editor-row')

const addRow = () => emit('update:rows', [...props.rows, { name: '', value: '' }])
const removeRow = (index: number) => emit('update:rows', props.rows.filter((_, rowIndex) => rowIndex !== index))
const fillTemplate = () => {
  const next = [...props.rows]
  const existing = new Set(next.map((row) => row.name.trim().toLowerCase()).filter(Boolean))
  for (const row of getHeaderOverrideTemplate(props.platform)) {
    if (!existing.has(row.name)) {
      next.push(row)
      existing.add(row.name)
    }
  }
  emit('update:rows', next)
}
</script>
