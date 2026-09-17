<template>
  <Select
    :model-value="modelValue"
    :options="options"
    placeholder="自动识别"
    @update:model-value="emit('update:modelValue', String($event ?? ''))"
  />
</template>

<script setup lang="ts">
import { computed } from 'vue'
import Select from '@/components/common/Select.vue'

const props = defineProps<{ modelValue: string }>()
const emit = defineEmits<{ (event: 'update:modelValue', value: string): void }>()

const options = computed(() => {
  const tiers = [
    { value: '', label: '自动识别' },
    { value: 'plus', label: 'Plus' },
    { value: 'pro', label: 'Pro' },
    { value: 'free', label: 'Free' },
    { value: 'team', label: 'Team / Business' },
    { value: 'business', label: 'Business' },
    { value: 'enterprise', label: 'Enterprise' },
    { value: 'edu', label: 'Edu' }
  ]
  // Keep newly introduced upstream tiers visible without changing their stored value.
  if (props.modelValue && !tiers.some(tier => tier.value === props.modelValue)) {
    tiers.push({ value: props.modelValue, label: `${props.modelValue}（当前档位）` })
  }
  return tiers
})
</script>
