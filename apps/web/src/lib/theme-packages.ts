import { request } from "./api"
import type { ThemePackageAppearance } from "./api-types"
import type { LayoutSurface } from "@/themes/layouts/catalog"

export const themePackagesKey = ["admin", "settings", "theme-packages"] as const
const base = "/api/admin/settings/theme-packages"

export type ThemePackage = {
  id: string
  name: string
  version: string
  apiVersion: 1
  surfaces: Partial<Record<LayoutSurface, ThemePackageAppearance>>
  source?: string
  release?: string
  previousVersion?: string
}

export type ThemePackageCatalog = {
  packages: ThemePackage[]
  active: Record<LayoutSurface, string>
  activeVersions?: Partial<Record<LayoutSurface, string>>
}

export const themePackageApi = {
  list: () => request<ThemePackageCatalog>(base),
  upload: (file: File) =>
    request<ThemePackageCatalog>(`${base}/upload`, {
      method: "POST",
      headers: { "Content-Type": "application/gzip" },
      body: file,
    }),
  github: (url: string, replace?: { id: string; expectedVersion: string }) =>
    request<ThemePackageCatalog>(`${base}/github${replace ? "?replace=true" : ""}`, {
      method: "POST",
      body: JSON.stringify({ url, ...replace }),
      timeoutMs: 25_000,
    }),
  activate: (surface: LayoutSurface, id: string, version?: string) =>
    request<ThemePackageCatalog>(`${base}/activate`, {
      method: "POST",
      body: JSON.stringify({ surface, id, version }),
    }),
  rollback: (payload: { id: string; expectedVersion: string; previousVersion: string }) =>
    request<ThemePackageCatalog>(`${base}/rollback`, {
      method: "POST",
      body: JSON.stringify(payload),
    }),
  remove: (id: string) =>
    request<ThemePackageCatalog>(`${base}/${encodeURIComponent(id)}`, { method: "DELETE" }),
}
