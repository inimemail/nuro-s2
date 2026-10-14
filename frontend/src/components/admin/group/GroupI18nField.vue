<script setup lang="ts">
import { computed, ref, useId } from 'vue'
import { useI18n } from 'vue-i18n'
import { availableLocales } from '@/i18n'
import type { GroupI18n } from '@/types'

const props = defineProps<{ modelValue: GroupI18n }>()
const emit = defineEmits<{ 'update:modelValue': [value: GroupI18n] }>()
const { t } = useI18n()
const selected = ref(availableLocales[0]?.code ?? 'zh')
const id = useId()
const text = computed(() => props.modelValue[selected.value] ?? {})
function update(field: 'name' | 'description', value: string) {
  emit('update:modelValue', { ...props.modelValue, [selected.value]: { ...text.value, [field]: value } })
}
</script>

<template>
  <details class="group rounded-xl border border-gray-200 bg-gray-50/60 dark:border-dark-600 dark:bg-dark-800/50">
    <summary class="cursor-pointer px-4 py-3 text-sm font-medium text-gray-700 focus-visible:outline-primary-500 dark:text-gray-200">
      {{ t('admin.groups.i18n.title') }}
      <span class="ml-2 text-xs font-normal text-gray-500 dark:text-gray-400">{{ t('admin.groups.i18n.optional') }}</span>
    </summary>
    <div class="space-y-3 border-t border-gray-200 p-4 dark:border-dark-600">
      <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('admin.groups.i18n.hint') }}</p>
      <label :for="`${id}-language`" class="input-label">{{ t('admin.groups.i18n.language') }}</label>
      <select :id="`${id}-language`" v-model="selected" class="input">
        <option v-for="locale in availableLocales" :key="locale.code" :value="locale.code">{{ locale.name }}</option>
      </select>
      <div>
        <label :for="`${id}-name`" class="input-label">{{ t('admin.groups.form.name') }}</label>
        <input :id="`${id}-name`" :value="text.name ?? ''" class="input" maxlength="100" @input="update('name', ($event.target as HTMLInputElement).value)" />
      </div>
      <div>
        <label :for="`${id}-description`" class="input-label">{{ t('admin.groups.form.description') }}</label>
        <textarea :id="`${id}-description`" :value="text.description ?? ''" class="input min-h-[88px]" maxlength="2000" @input="update('description', ($event.target as HTMLTextAreaElement).value)" />
      </div>
    </div>
  </details>
</template>
