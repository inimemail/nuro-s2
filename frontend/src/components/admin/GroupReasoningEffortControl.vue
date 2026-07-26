<template>
  <section class="border-t border-gray-200 pt-4 dark:border-dark-400">
    <div class="mb-4 flex items-start gap-3">
      <span class="flex h-8 w-8 flex-none items-center justify-center rounded-md bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300">
        <Icon name="brain" size="sm" :stroke-width="1.8" />
      </span>
      <div class="min-w-0">
        <h4 class="text-sm font-semibold text-gray-900 dark:text-gray-100">
          {{ t("admin.groups.form.reasoningEffortPolicy") }}
        </h4>
        <p class="mt-0.5 text-xs leading-5 text-gray-500 dark:text-gray-400">
          {{ t("admin.groups.form.maxReasoningEffortHint") }}
        </p>
      </div>
    </div>

    <div
      class="grid gap-3 border-y border-gray-200 bg-gray-50/70 px-3 py-3 dark:border-dark-500 dark:bg-dark-700/30 sm:grid-cols-[minmax(0,1fr)_minmax(12rem,18rem)] sm:items-center"
    >
      <div class="min-w-0">
        <label
          class="text-sm font-medium text-gray-700 dark:text-gray-300"
          :for="`${idPrefix}-max-reasoning-effort`"
        >
          {{ t("admin.groups.form.maxReasoningEffort") }}
        </label>
      </div>
      <select
        :id="`${idPrefix}-max-reasoning-effort`"
        :value="maxReasoningEffort"
        class="input"
        @change="updateMaxReasoningEffort"
      >
        <option value="">{{ t("admin.groups.form.maxReasoningEffortUnlimited") }}</option>
        <option v-for="value in values" :key="value" :value="value">
          {{ value }}
        </option>
      </select>
    </div>

    <div class="mt-5 flex items-center justify-between gap-3">
      <span class="text-xs font-semibold uppercase text-gray-500 dark:text-gray-400">
        {{ t("admin.groups.form.reasoningEffortMappings") }}
      </span>
      <span
        v-if="mappings.length > 0"
        class="text-xs tabular-nums text-gray-400 dark:text-gray-500"
      >
        {{ mappings.length }}
      </span>
    </div>

    <div v-if="mappings.length > 0" class="mt-2 space-y-2">
      <div
        v-for="(mapping, index) in mappings"
        :key="`${idPrefix}-reasoning-${index}`"
        class="rounded-lg border border-gray-200 bg-white p-3 dark:border-dark-500 dark:bg-dark-700/20"
      >
        <div class="mb-2 flex items-center justify-between gap-3">
          <span class="text-xs font-semibold tabular-nums text-gray-500 dark:text-gray-400">
            #{{ index + 1 }}
          </span>
          <button
            type="button"
            class="btn btn-ghost btn-icon h-8 w-8 text-gray-400 hover:bg-red-50 hover:text-red-600 dark:text-gray-500 dark:hover:bg-red-950/30 dark:hover:text-red-400"
            :title="t('admin.groups.form.removeReasoningEffortMapping')"
            :aria-label="t('admin.groups.form.removeReasoningEffortMapping')"
            :data-testid="`${idPrefix}-remove-reasoning-${index}`"
            @click="removeMapping(index)"
          >
            <Icon name="trash" size="sm" />
          </button>
        </div>

        <div class="grid grid-cols-[minmax(0,1fr)_1.75rem_minmax(0,1fr)] items-end gap-2">
          <label class="block min-w-0 text-xs font-medium text-gray-500 dark:text-gray-400">
            <span class="mb-1 block">{{ t("admin.groups.form.reasoningEffortFrom") }}</span>
            <select
              :value="mapping.from"
              class="input"
              :data-testid="`${idPrefix}-reasoning-from-${index}`"
              @change="updateMapping(index, 'from', $event)"
            >
              <option value="">{{ t("admin.groups.form.reasoningEffortFrom") }}</option>
              <option v-for="value in values" :key="value" :value="value">
                {{ value }}
              </option>
            </select>
          </label>

          <span class="flex h-10 items-center justify-center text-gray-400 dark:text-gray-500">
            <Icon name="arrowRight" size="sm" />
          </span>

          <label class="block min-w-0 text-xs font-medium text-gray-500 dark:text-gray-400">
            <span class="mb-1 block">{{ t("admin.groups.form.reasoningEffortTo") }}</span>
            <select
              :value="mapping.to"
              class="input"
              :data-testid="`${idPrefix}-reasoning-to-${index}`"
              @change="updateMapping(index, 'to', $event)"
            >
              <option value="">{{ t("admin.groups.form.reasoningEffortTo") }}</option>
              <option v-for="value in values" :key="value" :value="value">
                {{ value }}
              </option>
            </select>
          </label>
        </div>
      </div>
    </div>

    <div
      v-else
      class="mt-2 border-y border-dashed border-gray-200 py-4 text-center text-sm text-gray-400 dark:border-dark-500 dark:text-gray-500"
    >
      {{ t("admin.groups.form.reasoningEffortMappingsEmpty") }}
    </div>

    <button
      type="button"
      class="mt-3 flex min-h-10 w-full items-center justify-center gap-2 rounded-md border border-dashed border-gray-300 px-3 py-2 text-sm font-medium text-gray-600 transition-colors hover:border-primary-400 hover:bg-primary-50 hover:text-primary-700 dark:border-dark-500 dark:text-gray-300 dark:hover:border-primary-500 dark:hover:bg-primary-950/20 dark:hover:text-primary-300"
      :data-testid="`${idPrefix}-add-reasoning-mapping`"
      @click="addMapping"
    >
      <Icon name="plus" size="sm" />
      {{ t("admin.groups.form.addReasoningEffortMapping") }}
    </button>
  </section>
</template>

<script setup lang="ts">
import { useI18n } from "vue-i18n";
import Icon from "@/components/icons/Icon.vue";
import type { ReasoningEffortMapping } from "@/types";

const props = defineProps<{
  idPrefix: string;
  maxReasoningEffort: string;
  mappings: ReasoningEffortMapping[];
  values: readonly string[];
}>();

const emit = defineEmits<{
  "update:maxReasoningEffort": [value: string];
  "update:mappings": [value: ReasoningEffortMapping[]];
}>();

const { t } = useI18n();

function selectValue(event: Event): string {
  return (event.target as HTMLSelectElement).value;
}

function updateMaxReasoningEffort(event: Event): void {
  emit("update:maxReasoningEffort", selectValue(event));
}

function updateMapping(index: number, field: keyof ReasoningEffortMapping, event: Event): void {
  const next = props.mappings.map((mapping, mappingIndex) =>
    mappingIndex === index ? { ...mapping, [field]: selectValue(event) } : mapping,
  );
  emit("update:mappings", next);
}

function addMapping(): void {
  emit("update:mappings", [...props.mappings, { from: "", to: "" }]);
}

function removeMapping(index: number): void {
  emit("update:mappings", props.mappings.filter((_, mappingIndex) => mappingIndex !== index));
}
</script>
