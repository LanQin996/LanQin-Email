import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { resolveThemeId, themes } from "@/themes/registry"

export function SiteTitleSync() {
  const settings = useQuery({
    queryKey: ["public-settings"],
    queryFn: api.publicSettings,
    refetchOnWindowFocus: true,
    refetchInterval: 60_000,
  })
  React.useEffect(() => {
    document.title = settings.data?.siteTitle || "LanQin Email"
  }, [settings.data?.siteTitle])
  React.useEffect(() => {
    document.documentElement.dataset.lanqinTheme = resolveThemeId(settings.data?.themeId, themes)
  }, [settings.data?.themeId])
  return null
}
