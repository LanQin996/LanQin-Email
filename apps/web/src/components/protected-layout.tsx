import { LanguageSelector } from "@/components/language-selector"
import { uiText, useLanguage as useUiLanguage } from "@/lib/language"
import * as React from "react"
import { Outlet, useLocation } from "react-router-dom"
import {
  BarChart3,
  Copy,
  DatabaseBackup,
  Globe2,
  Inbox,
  LogOut,
  Mail,
  Mailbox,
  Settings,
  ShieldCheck,
  Users,
} from "lucide-react"
import { useMe } from "@/hooks/use-me"
import { api } from "@/lib/api"
import { useQuery } from "@tanstack/react-query"
import { useLogout } from "@/hooks/use-logout"
import { AuthGuard } from "@/components/auth-guard"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { hasAdminAccess, hasAnyPermission } from "@/lib/permissions"
import type { User } from "@/lib/api-types"
import { LayoutBoundary } from "@/components/layout-boundary"
import { TemplateLayoutScope } from "@/components/template-layout-scope"
import { useTemplateLayout } from "@/lib/template-layout"
import {
  adminSectionKeys,
  adminSectionLabels,
  adminSectionPermissions,
  type AdminSection,
} from "@/components/admin-config"
import type { AdminShellSlots } from "@/themes/layouts/admin/contract"
import { AdminLayoutContext, getAdminLayoutTemplate } from "@/themes/layouts/admin/registry"

const adminNavigationIcons: Partial<Record<AdminSection, React.ReactNode>> = {
  overview: <BarChart3 aria-hidden />,
  users: <Users aria-hidden />,
  permissionGroups: <ShieldCheck aria-hidden />,
  domains: <Globe2 aria-hidden />,
  mailboxes: <Mailbox aria-hidden />,
  aliases: <Copy aria-hidden />,
  messages: <Inbox aria-hidden />,
  backups: <DatabaseBackup aria-hidden />,
  settings: <Settings aria-hidden />,
}

export function ProtectedLayout() {
  useUiLanguage()

  return (
    <AuthGuard>
      <ProtectedContent />
    </AuthGuard>
  )
}

function ProtectedContent() {
  const { data } = useMe()
  const { pathname } = useLocation()
  const user = data?.user

  if (!pathname.toLowerCase().startsWith("/admin")) {
    return (
      <TemplateLayoutScope key="front" surface="front">
        <Outlet />
      </TemplateLayoutScope>
    )
  }
  if (!user || !hasAdminAccess(user)) return <Outlet />
  return (
    <TemplateLayoutScope key="admin" surface="admin">
      <AdminProtectedContent user={user} />
    </TemplateLayoutScope>
  )
}

function AdminProtectedContent({ user }: { user: User }) {
  useUiLanguage()

  const publicSettings = useQuery({ queryKey: ["public-settings"], queryFn: api.publicSettings })
  const location = useLocation()
  const logout = useLogout()
  const layoutId = useTemplateLayout("admin")

  const visibleSections = adminSectionKeys.filter((key) =>
    key === "backups" ? user.role === "admin" : hasAnyPermission(user, adminSectionPermissions[key])
  )
  const searchParams = new URLSearchParams(location.search)
  const requestedSection = searchParams.get("section")
  const forceClassic = searchParams.get("layout") === "classic"
  const activeSection =
    visibleSections.find((key) => key === requestedSection) ?? visibleSections[0]

  const siteTitle = publicSettings.data?.siteTitle || "LanQin Email"
  const slots: AdminShellSlots = {
    brandLabel: siteTitle,
    accountLabel: `${uiText("个人中心")}：${user.displayName}`,
    brand: (
      <>
        <div className="flex aspect-square size-8 shrink-0 items-center justify-center rounded-lg bg-primary text-primary-foreground">
          <Mail className="size-4" aria-hidden />
        </div>
        <div className="grid min-w-0 flex-1 text-left text-sm leading-tight">
          <span className="truncate font-semibold">{siteTitle}</span>
        </div>
      </>
    ),
    sidebarLanguage: <LanguageSelector variant="sidebar" />,
    compactLanguage: <LanguageSelector />,
    account: (
      <>
        <Avatar className="h-8 w-8 shrink-0 rounded-lg">
          <AvatarFallback className="rounded-lg bg-muted text-foreground">
            {user.displayName.slice(0, 1).toUpperCase()}
          </AvatarFallback>
        </Avatar>
        <div
          data-admin-shell-detail
          className="grid min-w-0 flex-1 text-left text-sm leading-tight"
        >
          <span className="truncate font-semibold">{user.displayName}</span>
          <span className="truncate text-xs text-muted-foreground">{user.email}</span>
        </div>
        <Badge
          data-admin-shell-detail
          variant={user.role === "admin" ? "default" : "secondary"}
          className="ml-auto text-[10px]"
        >
          {user.role === "admin" ? uiText("超级管理员") : uiText("普通用户")}
        </Badge>
      </>
    ),
    signOut: (
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="w-full gap-2 text-xs"
        onClick={logout}
        aria-label={uiText("退出登录")}
      >
        <LogOut className="h-3.5 w-3.5" aria-hidden />
        <span data-admin-shell-detail>{uiText("退出登录")}</span>
      </Button>
    ),
    navigationLabel: uiText("系统管理"),
    openNavigationLabel: uiText("打开导航"),
    currentLabel: activeSection ? uiText(adminSectionLabels[activeSection]) : uiText("系统管理"),
    activeKey: activeSection ?? "",
    links: visibleSections.flatMap((key) => {
      const icon = adminNavigationIcons[key]
      return icon
        ? [
            {
              key,
              label: uiText(adminSectionLabels[key]),
              icon,
              href: `/admin?section=${key}${forceClassic ? "&layout=classic" : ""}`,
            },
          ]
        : []
    }),
    children: <Outlet />,
  }
  const { Shell } = getAdminLayoutTemplate(layoutId)
  const { Shell: ClassicShell } = getAdminLayoutTemplate("classic")

  return (
    <LayoutBoundary
      layoutId={layoutId}
      fallback={
        <AdminLayoutContext.Provider value="classic">
          <ClassicShell {...slots} />
        </AdminLayoutContext.Provider>
      }
    >
      <AdminLayoutContext.Provider value={layoutId}>
        <Shell {...slots} />
      </AdminLayoutContext.Provider>
    </LayoutBoundary>
  )
}
