export const DEFAULT_THEME_ID = "classic"

export type ThemeManifest = {
  version: 1
  id: string
  name: string
}

const colorTokens = [
  "background",
  "foreground",
  "card",
  "card-foreground",
  "popover",
  "popover-foreground",
  "primary",
  "primary-foreground",
  "secondary",
  "secondary-foreground",
  "muted",
  "muted-foreground",
  "accent",
  "accent-foreground",
  "destructive",
  "destructive-foreground",
  "border",
  "input",
  "ring",
  "sidebar-background",
  "sidebar-foreground",
  "sidebar-primary",
  "sidebar-primary-foreground",
  "sidebar-accent",
  "sidebar-accent-foreground",
  "sidebar-border",
  "sidebar-ring",
] as const

type ThemeToken = (typeof colorTokens)[number] | "radius"
type ThemeTokens = Partial<Record<ThemeToken, string>>
export type ThemePack = ThemeManifest & { light: ThemeTokens; dark: ThemeTokens }

const themeIdPattern = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/
const colorTokenSet = new Set<string>(colorTokens)
const packKeys = new Set(["version", "id", "name", "light", "dark"])
const number = "[0-9]{1,3}(?:\\.[0-9]{1,3})?"
const hslPattern = new RegExp(`^(${number}) (${number})% (${number})%$`)
const radiusPattern = new RegExp(`^(${number})rem$`)

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

function readTokens(value: unknown, path: string): ThemeTokens {
  if (!isObject(value)) throw new Error(`${path} must be an object`)
  const entries = Object.entries(value).map(([key, val]) => {
    const label = `${path}.${key}`
    if (key !== "radius" && !colorTokenSet.has(key)) {
      throw new Error(`${label} is not a supported theme token`)
    }
    if (typeof val !== "string") throw new Error(`${label} must be a string`)
    if (key === "radius") {
      const match = radiusPattern.exec(val)
      if (!match || Number(match[1]) > 2) {
        throw new Error(`${label} must be between 0rem and 2rem`)
      }
    } else {
      const match = hslPattern.exec(val)
      if (!match || Number(match[1]) > 360 || Number(match[2]) > 100 || Number(match[3]) > 100) {
        throw new Error(`${label} must be HSL channels: 0–360 0–100% 0–100%`)
      }
    }
    // Reject trailing newlines too ($ also matches before a final newline in JS).
    if (val.trim() !== val) throw new Error(`${label} must not have extra whitespace`)
    return [key, val]
  })
  return Object.fromEntries(entries)
}

export function defineTheme<T extends ThemeManifest>(manifest: T): T {
  if (
    manifest.version !== 1 ||
    manifest.id.length > 48 ||
    manifest.id.trim() !== manifest.id ||
    !themeIdPattern.test(manifest.id) ||
    !manifest.name.trim()
  ) {
    throw new Error(`Invalid v1 theme metadata: ${manifest.id}`)
  }
  return manifest
}

export function readThemePack(value: unknown, source: string): ThemePack {
  if (
    !isObject(value) ||
    value.version !== 1 ||
    typeof value.id !== "string" ||
    typeof value.name !== "string" ||
    Object.keys(value).some((key) => !packKeys.has(key))
  ) {
    throw new Error(`${source} must be a v1 theme pack with id, name, light, and dark`)
  }
  if (value.id === DEFAULT_THEME_ID) throw new Error(`${source} uses reserved theme ID classic`)
  const metadata = defineTheme({ version: 1, id: value.id, name: value.name })
  return {
    ...metadata,
    light: readTokens(value.light, `${source}.light`),
    dark: readTokens(value.dark, `${source}.dark`),
  }
}

export function validateThemeIds(themes: readonly ThemeManifest[]) {
  const seen = new Set<string>()
  for (const theme of themes) {
    if (seen.has(theme.id)) throw new Error(`Duplicate theme ID: ${theme.id}`)
    seen.add(theme.id)
  }
}

export function resolveThemeId(
  id: string | null | undefined,
  themes: readonly ThemeManifest[]
): string {
  return themes.find((theme) => theme.id === id)?.id ?? DEFAULT_THEME_ID
}

export function renderThemeCSS(packs: readonly ThemePack[]): string {
  function rule(selector: string, tokens: ThemeTokens) {
    const declarations = Object.entries(tokens).map(([key, value]) => `  --${key}: ${value};`)
    return declarations.length ? `${selector} {\n${declarations.join("\n")}\n}\n` : ""
  }
  return packs
    .map(
      ({ id, light, dark }) =>
        rule(`html:not(.dark)[data-lanqin-theme="${id}"]`, light) +
        rule(`html.dark[data-lanqin-theme="${id}"]`, dark)
    )
    .join("")
}
