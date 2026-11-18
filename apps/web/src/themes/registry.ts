import { classicTheme } from "./classic/manifest"
import { paperTheme } from "./paper/manifest"
import "./paper/styles.css"

export { DEFAULT_THEME_ID, resolveThemeId } from "./contract"

export const themes = [classicTheme, paperTheme] as const

if (new Set(themes.map(({ id }) => id)).size !== themes.length) {
  throw new Error("Duplicate theme ID")
}
