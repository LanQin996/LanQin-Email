export type LayoutSurface = "front" | "admin"
export const CLASSIC_LAYOUT = "classic"

export type LayoutManifest = {
  apiVersion: 1
  id: string
  name: string
  author: string
  surfaces: readonly LayoutSurface[]
}

export function isLayoutManifest(value: unknown): value is LayoutManifest {
  if (!value || typeof value !== "object") return false
  const item = value as Partial<Record<keyof LayoutManifest, unknown>>
  return (
    item.apiVersion === 1 &&
    typeof item.id === "string" &&
    item.id.length <= 48 &&
    /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/.test(item.id) &&
    item.id === item.id.trim() &&
    typeof item.name === "string" &&
    item.name.trim().length > 0 &&
    typeof item.author === "string" &&
    item.author.trim().length > 0 &&
    Array.isArray(item.surfaces) &&
    item.surfaces.length > 0 &&
    item.surfaces.every((surface) => surface === "front" || surface === "admin") &&
    new Set(item.surfaces).size === item.surfaces.length
  )
}

export function resolveLayoutFrom(
  catalog: readonly LayoutManifest[],
  id: string | undefined,
  surface: LayoutSurface
) {
  return (
    catalog.find((item) => item.id === id && item.surfaces.includes(surface))?.id ?? CLASSIC_LAYOUT
  )
}
