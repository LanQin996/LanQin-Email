import { classicTheme } from "./classic/manifest"
import { readThemePack, validateThemeIds } from "./contract"
import paper from "./paper/theme.json"

export const registeredPacks = [{ source: "src/themes/paper/theme.json", data: paper }]
export const themePacks = registeredPacks.map(({ source, data }) => readThemePack(data, source))
export const themes = [classicTheme, ...themePacks]

validateThemeIds(themes)
