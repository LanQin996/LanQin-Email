export const DEFAULT_THEME_ID = "classic"

export type ThemeManifest = {
  version: 1
  id: string
  name: string
}

const themeIdPattern = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/

export function defineTheme<T extends ThemeManifest>(manifest: T): T {
  if (manifest.id.length > 48 || !themeIdPattern.test(manifest.id)) {
    throw new Error(`Invalid theme ID: ${manifest.id}`)
  }
  return manifest
}

export function resolveThemeId(
  id: string | null | undefined,
  themes: readonly ThemeManifest[]
): string {
  return themes.find((theme) => theme.id === id)?.id ?? DEFAULT_THEME_ID
}
