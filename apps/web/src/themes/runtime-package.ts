import type { ThemePackageAppearance } from "../lib/api-types"
import { readThemeTokens } from "./contract"

export function normalizeThemePackage(value: unknown): ThemePackageAppearance | undefined {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return undefined
  const { layout, light, dark } = value as Partial<ThemePackageAppearance>
  if (layout !== "classic" && layout !== "workbench") return undefined
  try {
    return { layout, light: readThemeTokens(light, "light"), dark: readThemeTokens(dark, "dark") }
  } catch {
    return undefined
  }
}

export function themePackageVariables(
  appearance: ThemePackageAppearance,
  mode: "light" | "dark"
): Record<`--${string}`, string> {
  try {
    return Object.fromEntries(
      Object.entries(readThemeTokens(appearance[mode], mode)).map(([key, value]) => [
        `--${key}`,
        value,
      ])
    )
  } catch {
    return {}
  }
}

export function runtimeThemeCSS(appearance: ThemePackageAppearance): string {
  const rule = (selector: string, mode: "light" | "dark") =>
    `${selector} {${Object.entries(themePackageVariables(appearance, mode))
      .map(([property, value]) => `${property}: ${value};`)
      .join("")}}`
  // The theme applies to the active surface, including dialogs portaled into
  // the body. Specificity makes runtime tokens override the bundled palette.
  return (
    rule("html:root[data-lanqin-theme]:not(.dark)", "light") +
    rule("html:root[data-lanqin-theme].dark", "dark")
  )
}
