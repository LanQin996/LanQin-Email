import * as React from "react"

export type DisplayMode = "detailed" | "compact"

const DISPLAY_MODE_KEY = "lanqin:display-mode"

export function getInitialDisplayMode(): DisplayMode {
  if (typeof window === "undefined") return "detailed"
  return window.localStorage.getItem(DISPLAY_MODE_KEY) === "compact" ? "compact" : "detailed"
}

export function setStoredDisplayMode(mode: DisplayMode) {
  window.localStorage.setItem(DISPLAY_MODE_KEY, mode)
  window.dispatchEvent(new CustomEvent("lanqin:display-mode", { detail: mode }))
}

export function useDisplayMode() {
  const [displayMode, setDisplayModeState] = React.useState<DisplayMode>(getInitialDisplayMode)
  React.useEffect(() => {
    function sync() {
      setDisplayModeState(getInitialDisplayMode())
    }
    window.addEventListener("storage", sync)
    window.addEventListener("lanqin:display-mode", sync)
    return () => {
      window.removeEventListener("storage", sync)
      window.removeEventListener("lanqin:display-mode", sync)
    }
  }, [])
  const setDisplayMode = React.useCallback((mode: DisplayMode) => {
    setStoredDisplayMode(mode)
    setDisplayModeState(mode)
  }, [])
  return [displayMode, setDisplayMode] as const
}

