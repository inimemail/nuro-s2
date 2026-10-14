import { watch } from 'vue'
import { useI18n } from 'vue-i18n'

/** Refresh only localized read data. Form state stays owned by the page. */
export function useLocaleRefresh(refresh: () => unknown) {
  const { locale } = useI18n()
  watch(locale, () => { void refresh() })
}
