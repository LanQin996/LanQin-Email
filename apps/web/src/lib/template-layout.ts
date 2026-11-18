import { createContext, useContext } from "react"
import { CLASSIC_LAYOUT, type LayoutSurface } from "@/themes/layouts/catalog"

export const publicLayoutKey = ["public-settings"] as const
export const activeAdminLayoutKey = ["site-layout", "admin"] as const
export const managedLayoutsKey = ["admin", "settings", "layouts"] as const

export const TemplateLayoutContext = createContext<{ surface: LayoutSurface; id: string } | null>(
  null
)

export function useTemplateLayout(surface: LayoutSurface) {
  const active = useContext(TemplateLayoutContext)
  return active?.surface === surface ? active.id : CLASSIC_LAYOUT
}
