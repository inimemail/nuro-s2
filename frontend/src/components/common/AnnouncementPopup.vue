<template>
  <Teleport to="body">
    <Transition name="popup-fade">
      <div
        v-if="displayedAnnouncement"
        class="fixed inset-0 z-[120] flex items-center justify-center overflow-y-auto bg-black/60 p-3 backdrop-blur-sm sm:p-6"
      >
        <div
          class="max-h-[calc(100dvh-1.5rem)] w-full max-w-2xl overflow-hidden rounded-lg bg-white shadow-2xl ring-1 ring-black/10 dark:bg-dark-800 dark:ring-white/10 sm:max-h-[calc(100dvh-3rem)]"
          @click.stop
        >
          <div class="border-b border-gray-200 bg-gray-50 px-5 py-4 dark:border-dark-700 dark:bg-dark-900/40 sm:px-6 sm:py-5">
            <div class="flex items-start gap-3">
              <span class="flex h-9 w-9 flex-none items-center justify-center rounded-md bg-primary-100 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300">
                <Icon name="bell" size="md" />
              </span>
              <div class="min-w-0 flex-1">
                <div class="mb-1.5 flex flex-wrap items-center gap-2">
                  <span class="badge badge-warning">
                    {{ preview ? t('admin.announcements.preview') : t('announcements.unread') }}
                  </span>
                </div>
                <h2 class="break-words text-xl font-semibold leading-7 text-gray-900 dark:text-white">
                  {{ displayedAnnouncement.title }}
                </h2>
                <div class="mt-1.5 flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
                  <Icon name="clock" size="sm" />
                  <time>{{ formatRelativeWithDateTime(displayedAnnouncement.created_at) }}</time>
                </div>
              </div>
              <button
                type="button"
                class="btn btn-ghost btn-icon h-9 w-9 flex-none text-gray-500"
                :title="t('common.close')"
                :aria-label="t('common.close')"
                @click="handleDismiss"
              >
                <Icon name="x" size="sm" />
              </button>
            </div>
          </div>

          <div class="max-h-[60vh] overflow-y-auto bg-white px-5 py-5 dark:bg-dark-800 sm:px-6 sm:py-6">
            <div
              class="markdown-body prose prose-sm max-w-none break-words dark:prose-invert"
              v-html="renderedContent"
            ></div>
          </div>

          <div class="border-t border-gray-200 bg-gray-50 px-5 py-4 dark:border-dark-700 dark:bg-dark-900/40 sm:px-6">
            <div class="flex items-center justify-end">
              <button
                type="button"
                @click="handleDismiss"
                data-testid="announcement-popup-dismiss"
                class="btn btn-primary"
              >
                <Icon :name="preview ? 'x' : 'check'" size="sm" class="mr-1.5" />
                {{ preview ? t('common.close') : t('announcements.markRead') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import Icon from '@/components/icons/Icon.vue'
import { useAnnouncementStore } from '@/stores/announcements'
import { formatRelativeWithDateTime } from '@/utils/format'
import { acquireBodyScrollLock, releaseBodyScrollLock } from '@/utils/bodyScrollLock'
import type { Announcement, UserAnnouncement } from '@/types'
import '@/styles/announcement-markdown.css'

type PreviewAnnouncement = Pick<Announcement | UserAnnouncement, 'title' | 'content' | 'created_at'>

const props = withDefaults(defineProps<{
  announcement?: PreviewAnnouncement | null
  preview?: boolean
}>(), {
  announcement: null,
  preview: false,
})

const emit = defineEmits<{ close: [] }>()

const { t } = useI18n()
const announcementStore = useAnnouncementStore()
const displayedAnnouncement = computed(() => props.preview ? props.announcement : announcementStore.currentPopup)
const bodyScrollLockOwner = Symbol('announcement-popup')

marked.setOptions({
  breaks: true,
  gfm: true,
})

const renderedContent = computed(() => {
  const content = displayedAnnouncement.value?.content
  if (!content) return ''
  const html = marked.parse(content) as string
  return DOMPurify.sanitize(html)
})

function handleDismiss() {
  if (props.preview) {
    emit('close')
    return
  }
  announcementStore.dismissPopup()
}

watch(
  displayedAnnouncement,
  (popup) => {
    if (popup) {
      acquireBodyScrollLock(bodyScrollLockOwner)
    } else {
      releaseBodyScrollLock(bodyScrollLockOwner)
    }
  },
  { immediate: true }
)

onBeforeUnmount(() => {
  releaseBodyScrollLock(bodyScrollLockOwner)
})
</script>

<style scoped>
.popup-fade-enter-active {
  transition: all 0.3s cubic-bezier(0.16, 1, 0.3, 1);
}

.popup-fade-leave-active {
  transition: all 0.2s cubic-bezier(0.4, 0, 1, 1);
}

.popup-fade-enter-from,
.popup-fade-leave-to {
  opacity: 0;
}

.popup-fade-enter-from > div {
  transform: scale(0.94) translateY(-12px);
  opacity: 0;
}

.popup-fade-leave-to > div {
  transform: scale(0.96) translateY(-8px);
  opacity: 0;
}

/* Scrollbar Styling */
.overflow-y-auto::-webkit-scrollbar {
  width: 8px;
}

.overflow-y-auto::-webkit-scrollbar-track {
  background: transparent;
}

.overflow-y-auto::-webkit-scrollbar-thumb {
  background: #94a3b8;
  border-radius: 4px;
}

.dark .overflow-y-auto::-webkit-scrollbar-thumb {
  background: #4b5563;
}
</style>
