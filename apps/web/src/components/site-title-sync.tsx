import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { adminThemeKey, syncSiteTheme } from "@/lib/site-theme"

export function SiteTitleSync() {
  const client = useQueryClient()
  const settings = useQuery({
    queryKey: ["public-settings"],
    queryFn: api.publicSettings,
    refetchOnWindowFocus: true,
    refetchInterval: 60_000,
  })
  React.useEffect(() => {
    document.title = settings.data?.siteTitle || "LanQin Email"
  }, [settings.data?.siteTitle])
  const themeId = settings.data?.themeId
  const hasSettings = settings.data !== undefined
  const initialFetchFailed = settings.isLoadingError
  React.useLayoutEffect(() => {
    if (hasSettings || initialFetchFailed) syncSiteTheme(themeId)
    if (hasSettings) {
      // A different admin may have changed the theme since this tab loaded.
      void client.invalidateQueries({ queryKey: adminThemeKey })
    }
  }, [client, hasSettings, initialFetchFailed, themeId])
  return null
}
