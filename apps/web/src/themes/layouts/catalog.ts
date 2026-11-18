import { CLASSIC_LAYOUT, isLayoutManifest, resolveLayoutFrom, type LayoutSurface } from "./manifest"
import workbench from "./packs/workbench/theme.json"
import classic from "./packs/classic/theme.json"

export { CLASSIC_LAYOUT, type LayoutSurface } from "./manifest"

const entries: unknown[] = [classic, workbench]
const customEntries = entries.slice(1).filter(isLayoutManifest)
export const layoutCatalog = [
  {
    apiVersion: 1,
    id: CLASSIC_LAYOUT,
    name: "经典主题",
    author: "LanQin Email",
    surfaces: ["front", "admin"],
  } as const,
  ...customEntries.filter(
    (entry, index) =>
      entry.id !== CLASSIC_LAYOUT &&
      customEntries.findIndex((item) => item.id === entry.id) === index
  ),
]

export function supportsLayout(id: string | undefined, surface: LayoutSurface) {
  return (
    id !== undefined &&
    layoutCatalog.some((item) => item.id === id && item.surfaces.some((entry) => entry === surface))
  )
}

export function resolveLayout(id: string | undefined, surface: LayoutSurface) {
  return resolveLayoutFrom(layoutCatalog, id, surface)
}
