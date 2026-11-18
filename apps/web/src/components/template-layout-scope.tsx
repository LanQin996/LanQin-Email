import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { useLocation } from "react-router-dom"
import { api } from "@/lib/api"
import { activeAdminLayoutKey, publicLayoutKey, TemplateLayoutContext } from "@/lib/template-layout"
import { CLASSIC_LAYOUT, resolveLayout, type LayoutSurface } from "@/themes/layouts/catalog"

const initialSettingsWaitMs = 1_200

export function TemplateLayoutScope({
  surface,
  children,
}: {
  surface: LayoutSurface
  children: React.ReactNode
}) {
  const { search } = useLocation()
  const forceClassic = new URLSearchParams(search).get("layout") === CLASSIC_LAYOUT
  return (
    <TemplateLayoutSession
      key={`${surface}:${forceClassic ? "recovery" : "normal"}`}
      surface={surface}
      forceClassic={forceClassic}
    >
      {children}
    </TemplateLayoutSession>
  )
}

function TemplateLayoutSession({
  surface,
  forceClassic,
  children,
}: {
  surface: LayoutSurface
  forceClassic: boolean
  children: React.ReactNode
}) {
  const [snapshot, setSnapshot] = React.useState<string>()
  const front = useQuery({
    queryKey: publicLayoutKey,
    queryFn: api.publicSettings,
    enabled: surface === "front" && !forceClassic,
    refetchOnMount: "always",
    select: (settings) => settings.frontLayoutId,
  })
  const admin = useQuery({
    queryKey: activeAdminLayoutKey,
    queryFn: api.siteLayout,
    enabled: surface === "admin" && !forceClassic,
    refetchOnMount: "always",
    refetchOnWindowFocus: false,
    retry: false,
    select: (settings) => settings.adminLayoutId,
  })
  const current = surface === "front" ? front : admin
  const availableLayout = resolveLayout(current.data, surface)

  React.useLayoutEffect(() => {
    if (current.isFetchedAfterMount) {
      setSnapshot((previous) => previous ?? (current.isError ? CLASSIC_LAYOUT : availableLayout))
    }
  }, [availableLayout, current.isError, current.isFetchedAfterMount])
  React.useEffect(() => {
    if (forceClassic || snapshot !== undefined) return
    // Bound the initial wait so slow preferences cannot block an entire page.
    const timer = window.setTimeout(() => {
      setSnapshot((previous) => previous ?? availableLayout)
    }, initialSettingsWaitMs)
    return () => window.clearTimeout(timer)
  }, [availableLayout, forceClassic, snapshot])

  // Preserve the mounted application tree when a later poll or settings save
  // returns a different template. Re-entry or an explicit reload adopts it.
  const id = forceClassic ? CLASSIC_LAYOUT : snapshot
  if (id === undefined) {
    return <div className="min-h-svh bg-background" aria-hidden="true" />
  }
  return (
    <TemplateLayoutContext.Provider value={{ surface, id }}>
      {children}
    </TemplateLayoutContext.Provider>
  )
}
