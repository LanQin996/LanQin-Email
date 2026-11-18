const THEME_TRANSITION_MS = 180
let themeTimer: number | undefined
const colorModeKey = "lanqin:theme"

export function getInitialTheme() {
  try {
    const saved = localStorage.getItem(colorModeKey)
    if (saved === "dark" || saved === "light") return saved === "dark"
  } catch {
    // Preserve the current in-memory mode when storage is unavailable.
  }
  return document.documentElement.classList.contains("dark")
}

export function initializeColorMode() {
  document.documentElement.classList.toggle("dark", getInitialTheme())
}

export function applyTheme(dark: boolean, animated = false) {
  const root = document.documentElement
  const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches
  const updateTheme = () => {
    root.classList.toggle("dark", dark)
    root.style.colorScheme = dark ? "dark" : "light"
    try {
      localStorage.setItem(colorModeKey, dark ? "dark" : "light")
    } catch {
      // Keep the current mode even if the browser cannot persist it.
    }
  }

  if (!animated || reduceMotion) {
    root.classList.remove("theme-transition")
    root.style.removeProperty("--theme-fade-bg")
    updateTheme()
    return
  }

  if (themeTimer) window.clearTimeout(themeTimer)
  root.style.setProperty("--theme-fade-bg", getComputedStyle(document.body).backgroundColor)
  root.classList.add("theme-transition")
  requestAnimationFrame(() => {
    updateTheme()
    themeTimer = window.setTimeout(() => {
      root.classList.remove("theme-transition")
      root.style.removeProperty("--theme-fade-bg")
      themeTimer = undefined
    }, THEME_TRANSITION_MS)
  })
}
