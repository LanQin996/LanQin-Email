const THEME_TRANSITION_MS = 180
let themeTimer: number | undefined

export function getInitialTheme() {
  return (
    localStorage.getItem("lanqin:theme") === "dark" ||
    document.documentElement.classList.contains("dark")
  )
}

export function applyTheme(dark: boolean, animated = false) {
  const root = document.documentElement
  const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches
  const updateTheme = () => {
    root.classList.toggle("dark", dark)
    root.style.colorScheme = dark ? "dark" : "light"
    localStorage.setItem("lanqin:theme", dark ? "dark" : "light")
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
