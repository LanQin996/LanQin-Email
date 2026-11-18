import { DEFAULT_THEME_ID, resolveThemeId, themes } from "@/themes/registry"

export const adminThemeKey = ["admin", "settings", "theme"] as const

const siteThemeCacheKey = "lanqin:site-theme"

export function initializeSiteTheme() {
  let cached: string | null = null
  try {
    cached = localStorage.getItem(siteThemeCacheKey)
  } catch {
    // Browser storage is optional; fetch the current preference normally.
  }
  document.documentElement.dataset.lanqinTheme = resolveThemeId(cached, themes)
}

export function syncSiteTheme(id: string | undefined) {
  const resolved = resolveThemeId(id, themes)
  document.documentElement.dataset.lanqinTheme = resolved
  try {
    if (resolved === DEFAULT_THEME_ID) {
      localStorage.removeItem(siteThemeCacheKey)
    } else {
      localStorage.setItem(siteThemeCacheKey, resolved)
    }
  } catch {
    // The applied theme doesn't depend on browser storage.
  }
}
