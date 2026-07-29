<template>
  <AppLayout v-if="embedded">
    <ModelPlazaContent :response="data" :loading="loading" :error="failed" :authenticated="authStore.isAuthenticated" embedded @retry="load" />
  </AppLayout>
  <div v-else class="min-h-screen bg-gray-50 dark:bg-dark-950">
    <nav class="border-b border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900">
      <div class="mx-auto flex h-16 max-w-[1500px] items-center justify-between px-4 sm:px-6">
        <router-link to="/home" class="flex min-w-0 items-center gap-3 font-semibold text-gray-900 dark:text-white">
          <img v-if="appStore.siteLogo" :src="appStore.siteLogo" class="h-8 w-8 object-contain" alt="" />
          <span class="truncate">{{ appStore.siteName }}</span>
        </router-link>
        <router-link :to="authStore.isAuthenticated ? '/dashboard' : '/login'" class="btn btn-secondary btn-sm">
          {{ authStore.isAuthenticated ? t('modelPlaza.nav.backToDashboard') : t('modelPlaza.nav.login') }}
        </router-link>
      </div>
    </nav>
    <main class="px-4 py-6 sm:px-6 lg:py-8">
      <ModelPlazaContent :response="data" :loading="loading" :error="failed" :authenticated="authStore.isAuthenticated" @retry="load" />
    </main>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import ModelPlazaContent from '@/components/modelPlaza/ModelPlazaContent.vue'
import { getModelPlaza, type ModelPlazaResponse } from '@/api/modelPlaza'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'

const { t } = useI18n()
const route = useRoute()
const appStore = useAppStore()
const authStore = useAuthStore()
const embedded = computed(() => route.query.embedded === '1' && authStore.isAuthenticated)
const data = ref<ModelPlazaResponse | null>(null)
const loading = ref(true)
const failed = ref(false)
async function load(): Promise<void> {
  loading.value = true
  failed.value = false
  try { data.value = await getModelPlaza() } catch { failed.value = true } finally { loading.value = false }
}
onMounted(() => { void appStore.fetchPublicSettings(); void load() })
</script>
