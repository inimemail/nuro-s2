<template>
  <div :class="props.embedded ? 'space-y-4' : 'card'">
    <div
      v-if="!props.embedded"
      class="border-b border-gray-100 px-6 py-4 dark:border-dark-700"
    >
      <h2 class="text-lg font-medium text-gray-900 dark:text-white">
        {{ t('profile.editProfile') }}
      </h2>
    </div>
    <div :class="props.embedded ? '' : 'px-6 py-6'">
      <form @submit.prevent="handleUpdateProfile" class="space-y-4">
        <div v-if="props.embedded">
          <p class="text-sm font-semibold text-gray-900 dark:text-white">
            {{ t('profile.editProfile') }}
          </p>
        </div>
        <div>
          <label for="username" class="input-label">
            {{ t('profile.username') }}
          </label>
          <input
            id="username"
            v-model="username"
            type="text"
            class="input"
            :placeholder="t('profile.enterUsername')"
          />
        </div>

        <div class="flex justify-end pt-4">
          <button type="submit" :disabled="loading" class="btn btn-primary">
            {{ loading ? t('profile.updating') : t('profile.updateProfile') }}
          </button>
        </div>
      </form>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { useAppStore } from '@/stores/app'
import { userAPI } from '@/api'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = withDefaults(defineProps<{
  initialUsername: string
  embedded?: boolean
}>(), {
  embedded: false,
})

const { t } = useI18n()
const authStore = useAuthStore()
const appStore = useAppStore()

const username = ref(props.initialUsername)
const loading = ref(false)
let session = 0
let disposed = false
onUnmounted(() => { disposed = true; session++ })
watch(() => [authStore.user?.id, authStore.token] as const, (current, previous) => {
  if (current[0] === previous[0] && current[1] === previous[1]) return
  session++
  loading.value = false
  username.value = authStore.user?.username ?? props.initialUsername
}, { flush: 'sync' })

watch(() => props.initialUsername, (val, previous) => {
  if (username.value === previous) username.value = val
})

const handleUpdateProfile = async () => {
  if (loading.value || disposed) return
  if (!username.value.trim()) {
    appStore.showError(t('profile.usernameRequired'))
    return
  }

  loading.value = true
  const version = session
  const isCurrent = () => !disposed && version === session
  const submittedUsername = username.value
  try {
    const updatedUser = await userAPI.updateProfile({
      username: submittedUsername
    })
    if (!isCurrent()) return
    authStore.user = updatedUser
    if (username.value === submittedUsername) username.value = updatedUser.username
    appStore.showSuccess(t('profile.updateSuccess'))
  } catch (error: any) {
    if (!isCurrent()) return
    appStore.showError(extractApiErrorMessage(error, t('profile.updateFailed')))
  } finally {
    if (isCurrent()) loading.value = false
  }
}
</script>
