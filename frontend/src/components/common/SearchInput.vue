<template>
  <div class="relative w-full">
    <div class="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-3">
      <Icon name="search" size="md" class="text-gray-400" />
    </div>
    <input
      v-model="searchValue"
      @compositionstart="cancelPendingSearch"
      type="text"
      class="input pl-10"
      :placeholder="placeholder"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount } from 'vue'
import Icon from '@/components/icons/Icon.vue'

const props = withDefaults(defineProps<{
  modelValue: string
  placeholder?: string
  debounceMs?: number
}>(), {
  placeholder: 'Search...',
  debounceMs: 300
})

const emit = defineEmits<{
  (e: 'update:modelValue', value: string): void
  (e: 'search', value: string): void
}>()

let timer: ReturnType<typeof setTimeout> | undefined
const cancelPendingSearch = () => clearTimeout(timer)
onBeforeUnmount(cancelPendingSearch)

// Vue's text v-model commits once at compositionend.
const searchValue = computed({
  get: () => props.modelValue,
  set: (value: string) => {
    emit('update:modelValue', value)
    clearTimeout(timer)
    timer = setTimeout(() => emit('search', value), props.debounceMs)
  }
})
</script>
