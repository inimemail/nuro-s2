<template>
  <section class="card overflow-hidden">
    <header class="border-b border-gray-100 px-4 py-4 dark:border-dark-700 sm:px-6">
      <div class="flex items-start gap-3">
        <span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-emerald-50 text-emerald-600 dark:bg-emerald-500/10 dark:text-emerald-300">
          <Icon name="shield" size="md" />
        </span>
        <div class="min-w-0">
          <h2 class="text-base font-semibold text-gray-900 dark:text-white sm:text-lg">
            {{ t("admin.settings.panelRateLimit.title") }}
          </h2>
          <p class="mt-1 text-sm leading-5 text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.panelRateLimit.description") }}
          </p>
        </div>
      </div>
    </header>

    <div class="space-y-5 p-4 sm:p-6">
      <div v-if="loading" class="flex min-h-24 items-center justify-center gap-2 text-sm text-gray-500 dark:text-gray-400">
        <span class="h-4 w-4 animate-spin rounded-full border-2 border-gray-200 border-t-primary-600 dark:border-dark-600 dark:border-t-primary-400" />
        {{ t("common.loading") }}
      </div>

      <template v-else>
        <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div class="min-w-0">
            <label class="font-medium text-gray-900 dark:text-white">{{ t("admin.settings.panelRateLimit.enabled") }}</label>
            <p class="mt-0.5 text-sm leading-5 text-gray-500 dark:text-gray-400">{{ t("admin.settings.panelRateLimit.enabledHint") }}</p>
          </div>
          <Toggle v-model="form.enabled" class="shrink-0" />
        </div>

        <div v-if="form.enabled" class="grid grid-cols-1 gap-4 border-t border-gray-100 pt-5 dark:border-dark-700 md:grid-cols-3">
          <label v-for="field in numericFields" :key="field.key" class="min-w-0">
            <span class="block text-sm font-medium text-gray-700 dark:text-gray-300">{{ field.label }}</span>
            <div class="mt-2 flex items-center overflow-hidden rounded-md border border-gray-300 bg-white focus-within:border-primary-500 focus-within:ring-1 focus-within:ring-primary-500 dark:border-dark-600 dark:bg-dark-800">
              <input
                v-model.number="form[field.key]"
                :data-testid="`panel-rate-limit-${field.key}`"
                type="number"
                min="0"
                max="100000"
                class="min-w-0 flex-1 border-0 bg-transparent px-3 py-2 text-sm text-gray-900 outline-none dark:text-white"
              />
              <span class="shrink-0 border-l border-gray-200 px-2 text-xs text-gray-500 dark:border-dark-600 dark:text-gray-400">{{ t("admin.settings.panelRateLimit.perMinute") }}</span>
            </div>
            <span class="mt-1.5 block text-xs leading-4 text-gray-500 dark:text-gray-400">{{ field.hint }}</span>
          </label>
        </div>

        <div v-if="form.enabled" class="flex flex-col gap-3 border-t border-gray-100 pt-5 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <label class="font-medium text-gray-900 dark:text-white">{{ t("admin.settings.panelRateLimit.exemptAdmin") }}</label>
            <p class="mt-0.5 text-sm leading-5 text-gray-500 dark:text-gray-400">{{ t("admin.settings.panelRateLimit.exemptAdminHint") }}</p>
          </div>
          <Toggle v-model="form.exempt_admin" class="shrink-0" />
        </div>

        <div class="flex justify-end border-t border-gray-100 pt-4 dark:border-dark-700">
          <button class="btn btn-primary btn-sm inline-flex items-center gap-2" type="button" :disabled="saving" data-testid="panel-rate-limit-save" @click="save">
            <span v-if="saving" class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white" />
            <Icon v-else name="check" size="sm" />
            {{ saving ? t("common.saving") : t("common.save") }}
          </button>
        </div>
      </template>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { settingsAPI, type PanelRateLimitSettings } from "@/api/admin/settings";
import Icon from "@/components/icons/Icon.vue";
import Toggle from "@/components/common/Toggle.vue";
import { useAppStore } from "@/stores/app";
import { extractApiErrorMessage } from "@/utils/apiError";

const { t } = useI18n();
const appStore = useAppStore();
const loading = ref(true);
const saving = ref(false);
const form = reactive<PanelRateLimitSettings>({
  enabled: true,
  user_rpm: 240,
  heavy_rpm: 60,
  exempt_admin: true,
  public_ip_rpm: 300,
});

const numericFields = computed(() => [
  { key: "user_rpm" as const, label: t("admin.settings.panelRateLimit.userRpm"), hint: t("admin.settings.panelRateLimit.userRpmHint") },
  { key: "heavy_rpm" as const, label: t("admin.settings.panelRateLimit.heavyRpm"), hint: t("admin.settings.panelRateLimit.heavyRpmHint") },
  { key: "public_ip_rpm" as const, label: t("admin.settings.panelRateLimit.publicIpRpm"), hint: t("admin.settings.panelRateLimit.publicIpRpmHint") },
]);

function normalize(): void {
  for (const field of numericFields.value) {
    const value = Number(form[field.key]);
    form[field.key] = Number.isFinite(value) ? Math.min(100000, Math.max(0, Math.floor(value))) : 0;
  }
}

async function load(): Promise<void> {
  loading.value = true;
  try {
    Object.assign(form, await settingsAPI.getPanelRateLimitSettings());
  } catch {
    // Defaults remain visible when an older backend does not expose this endpoint.
  } finally {
    loading.value = false;
  }
}

async function save(): Promise<void> {
  normalize();
  saving.value = true;
  try {
    Object.assign(form, await settingsAPI.updatePanelRateLimitSettings({ ...form }));
    appStore.showSuccess(t("admin.settings.panelRateLimit.saved"));
  } catch (error: unknown) {
    appStore.showError(extractApiErrorMessage(error, t("admin.settings.panelRateLimit.saveFailed")));
  } finally {
    saving.value = false;
  }
}

onMounted(load);
</script>
