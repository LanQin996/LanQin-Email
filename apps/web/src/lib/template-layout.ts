import { useQuery } from "@tanstack/react-query"
import { useLocation } from "react-router-dom"
import { api } from "@/lib/api"
import { CLASSIC_LAYOUT, resolveLayout, type LayoutSurface } from "@/themes/layouts/catalog"

export const publicLayoutKey = ["public-settings"] as const
export const activeAdminLayoutKey = ["site-layout", "admin"] as const
export const managedLayoutsKey = ["admin", "settings", "layouts"] as const

export function useTemplateLayout(surface: LayoutSurface) {
  const { search } = useLocation()
  const forceClassic = new URLSearchParams(search).get("layout") === CLASSIC_LAYOUT
  const front = useQuery({
    queryKey: publicLayoutKey,
    queryFn: api.publicSettings,
    enabled: surface === "front" && !forceClassic,
    select: (settings) => settings.frontLayoutId,
  })
  const admin = useQuery({
    queryKey: activeAdminLayoutKey,
    queryFn: api.siteLayout,
    enabled: surface === "admin" && !forceClassic,
    refetchOnWindowFocus: true,
    refetchInterval: 60_000,
    retry: false,
    select: (settings) => settings.adminLayoutId,
  })
  if (forceClassic) return CLASSIC_LAYOUT
  return resolveLayout(surface === "front" ? front.data : admin.data, surface)
}
