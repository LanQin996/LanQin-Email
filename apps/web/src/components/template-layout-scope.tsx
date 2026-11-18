import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { useLocation } from "react-router-dom"
import { api, type ThemePackageAppearance } from "@/lib/api"
import { activeAdminLayoutKey, publicLayoutKey, TemplateLayoutContext } from "@/lib/template-layout"
import { CLASSIC_LAYOUT, resolveLayout, type LayoutSurface } from "@/themes/layouts/catalog"
import { normalizeThemePackage, runtimeThemeCSS } from "@/themes/runtime-package"

type LayoutSnapshot = { id: string; themePackage?: ThemePackageAppearance }
const classicSnapshot: LayoutSnapshot = { id: CLASSIC_LAYOUT }

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
  const [snapshot, setSnapshot] = React.useState<LayoutSnapshot>()
  const front = useQuery({
    queryKey: publicLayoutKey,
    queryFn: api.publicSettings,
    enabled: surface === "front" && !forceClassic,
    refetchOnMount: "always",
  })
  const admin = useQuery({
    queryKey: activeAdminLayoutKey,
    queryFn: api.siteLayout,
    enabled: surface === "admin" && !forceClassic,
    refetchOnMount: "always",
    refetchOnWindowFocus: false,
    retry: false,
  })
  const current = surface === "front" ? front : admin
  const frontLayoutId = front.data?.frontLayoutId
  const adminLayoutId = admin.data?.adminLayoutId
  const data = current.data?.themePackage
  const availableLayout = React.useMemo(() => {
    const themePackage = normalizeThemePackage(data)
    return {
      id: resolveLayout(
        themePackage?.layout ?? (surface === "front" ? frontLayoutId : adminLayoutId),
        surface
      ),
      themePackage,
    }
  }, [adminLayoutId, data, frontLayoutId, surface])

  React.useLayoutEffect(() => {
    if (!forceClassic && current.isFetchedAfterMount) {
      setSnapshot((previous) => previous ?? (current.isError ? classicSnapshot : availableLayout))
    }
  }, [availableLayout, current.isError, current.isFetchedAfterMount, forceClassic])
  React.useEffect(() => {
    if (forceClassic || snapshot !== undefined) return
    // Bound the initial wait so slow preferences cannot block an entire page.
    const timer = window.setTimeout(() => {
      setSnapshot((previous) => previous ?? availableLayout)
    }, initialSettingsWaitMs)
    return () => window.clearTimeout(timer)
  }, [availableLayout, forceClassic, snapshot])

  const active = forceClassic ? classicSnapshot : snapshot
  const activePackage = active?.themePackage
  React.useLayoutEffect(() => {
    if (!activePackage) return
    const style = document.createElement("style")
    style.dataset.lanqinRuntimeTheme = surface
    style.textContent = runtimeThemeCSS(activePackage)
    document.head.append(style)
    return () => style.remove()
  }, [activePackage, surface])

  // Preserve the mounted application tree when a later poll or settings save
  // returns a different template. Re-entry or an explicit reload adopts it.
  if (active === undefined) {
    return <div className="min-h-svh bg-background" aria-hidden="true" />
  }
  return (
    <TemplateLayoutContext.Provider value={{ surface, id: active.id }}>
      {children}
    </TemplateLayoutContext.Provider>
  )
}
