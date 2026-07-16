<template>
  <BaseDialog :show="controller.visible.value" :title="t('stepUp.title')" width="narrow" @close="cancel">
    <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('stepUp.hint') }}</p>
    <div class="relative mt-4">
      <Icon name="shield" size="sm" class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-400" />
      <input ref="input" v-model.trim="code" class="input pl-9 text-center text-lg" inputmode="numeric" maxlength="6" autocomplete="one-time-code" :disabled="verifying" @keyup.enter="verify" />
    </div>
    <template #footer>
      <button class="btn btn-secondary" type="button" :disabled="verifying" @click="cancel">{{ t('common.cancel') }}</button>
      <button class="btn btn-primary" type="button" :disabled="verifying || code.length !== 6" @click="verify">{{ verifying ? t('common.verifying') : t('common.confirm') }}</button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { nextTick, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { totpAPI } from '@/api'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import type { StepUpController } from '@/composables/useStepUp'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ controller: StepUpController }>()
const { t } = useI18n()
const appStore = useAppStore()
const code = ref('')
const verifying = ref(false)
const input = ref<HTMLInputElement | null>(null)
watch(() => props.controller.visible.value, (visible) => { if (visible) { code.value = ''; nextTick(() => input.value?.focus()) } })
function cancel() { if (!verifying.value) props.controller.onCancel() }
async function verify() {
  if (code.value.length !== 6 || verifying.value) return
  verifying.value = true
  try { await totpAPI.stepUp(code.value); props.controller.onVerified() }
  catch (error) { appStore.showError(extractApiErrorMessage(error, t('stepUp.verifyFailed'))); code.value = ''; nextTick(() => input.value?.focus()) }
  finally { verifying.value = false }
}
</script>
