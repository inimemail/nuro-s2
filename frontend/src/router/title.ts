import { i18n } from '@/i18n'
import type { RouteLocationNormalized } from 'vue-router'
import type { CustomMenuItem } from '@/types'

/**
 * 统一生成页面标题，避免多处写入 document.title 产生覆盖冲突。
 * 优先使用 titleKey 通过 i18n 翻译，fallback 到静态 routeTitle。
 */
export function resolveDocumentTitle(routeTitle: unknown, siteName?: string, titleKey?: string): string {
  const normalizedSiteName = typeof siteName === 'string' && siteName.trim() ? siteName.trim() : 'Sub2API'

  if (typeof titleKey === 'string' && titleKey.trim()) {
    const translated = i18n.global.t(titleKey)
    if (translated && translated !== titleKey) {
      return `${translated} - ${normalizedSiteName}`
    }
  }

  if (typeof routeTitle === 'string' && routeTitle.trim()) {
    return `${routeTitle.trim()} - ${normalizedSiteName}`
  }

  return normalizedSiteName
}

export function resolveRouteDocumentTitle(
  route: RouteLocationNormalized,
  siteName?: string,
  customMenuItems: CustomMenuItem[] = []
): string {
  if (route.name === 'CustomPage') {
    const id = route.params.id as string | undefined
    const menuItem = id ? customMenuItems.find((item) => item.id === id) : undefined
    if (menuItem?.label?.trim()) {
      const normalizedSiteName = typeof siteName === 'string' && siteName.trim() ? siteName.trim() : 'Sub2API'
      return `${menuItem.label.trim()} - ${normalizedSiteName}`
    }
  }

  return resolveDocumentTitle(route.meta.title, siteName, route.meta.titleKey as string)
}
