import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { api } from "@/lib/api"

export function SiteTitleSync() {
  const settings = useQuery({ queryKey: ["public-settings"], queryFn: api.publicSettings })
  React.useEffect(() => {
    document.title = settings.data?.siteTitle || "LanQin Email"
  }, [settings.data?.siteTitle])
  return null
}
