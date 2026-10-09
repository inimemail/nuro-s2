import { computed, ref } from 'vue'
import { keysAPI } from '@/api/keys'
import { useAuthStore } from '@/stores/auth'
import type { ApiKey } from '@/types'

const loaded = ref(false)
const loading = ref(false)
const hasAllowedBatchImageKey = ref(false)
let pendingLoad: Promise<boolean> | null = null
let ownerToken: string | null = null
let loadVersion = 0
const pageSize = 100

function keyAllowsBatchImage(key: ApiKey): boolean {
  return (
    key.status === 'active' &&
    key.group?.platform === 'gemini' &&
    key.group?.allow_batch_image_generation === true
  )
}

async function loadBatchImageAccess(force = false): Promise<boolean> {
  const authStore = useAuthStore()
  if (ownerToken !== authStore.token || !authStore.isAuthenticated) {
    loadVersion++
    ownerToken = authStore.token
    loaded.value = false
    loading.value = false
    hasAllowedBatchImageKey.value = false
    pendingLoad = null
  }
  if (!authStore.isAuthenticated) {
    loaded.value = true
    hasAllowedBatchImageKey.value = false
    return false
  }

  if (loaded.value && !force) {
    return hasAllowedBatchImageKey.value
  }

  if (pendingLoad && !force) {
    return pendingLoad
  }

  loading.value = true
  const version = ++loadVersion
  const token = authStore.token
  const isCurrent = () => version === loadVersion && authStore.isAuthenticated && authStore.token === token
  pendingLoad = (async () => {
    let page = 1
    while (true) {
      const response = await keysAPI.list(page, pageSize, {
        status: 'active',
        sort_by: 'created_at',
        sort_order: 'desc'
      })
      if (!isCurrent()) return false

      if ((response.items || []).some(keyAllowsBatchImage)) {
        hasAllowedBatchImageKey.value = true
        loaded.value = true
        return true
      }

      if (page >= response.pages || (response.items || []).length === 0) {
        hasAllowedBatchImageKey.value = false
        loaded.value = true
        return false
      }

      page += 1
    }
  })()
    .catch(() => {
      if (!isCurrent()) return false
      hasAllowedBatchImageKey.value = false
      loaded.value = false
      return false
    })
    .finally(() => {
      if (!isCurrent()) return
      loading.value = false
      pendingLoad = null
    })

  return pendingLoad
}

export function useBatchImageAccess() {
  const authStore = useAuthStore()
  const ownsState = computed(() => authStore.isAuthenticated && authStore.token === ownerToken)
  const canUseBatchImage = computed(() => ownsState.value && hasAllowedBatchImageKey.value)

  return {
    canUseBatchImage,
    batchImageAccessLoaded: computed(() => ownsState.value && loaded.value),
    batchImageAccessLoading: computed(() => ownsState.value && loading.value),
    refreshBatchImageAccess: loadBatchImageAccess,
  }
}
