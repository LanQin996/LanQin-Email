import * as React from "react"
import { Outlet, Link, useLocation } from "react-router-dom"
import {
  BarChart3,
  Copy,
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
import { useLogout } from "@/hooks/use-logout"
import { AuthGuard } from "@/components/auth-guard"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { hasAnyPermission } from "@/lib/permissions"
import type { PermissionKey } from "@/lib/api-types"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarRail,
  SidebarTrigger,
  useSidebar,
} from "@/components/ui/sidebar"

const adminSections: {
  key: string
  label: string
  icon: React.ReactNode
  permissions: PermissionKey[]
}[] = [
  { key: "overview", label: "概览", icon: <BarChart3 />, permissions: ["admin.overview.view"] },
  { key: "users", label: "用户", icon: <Users />, permissions: ["admin.users.view"] },
  {
    key: "permissionGroups",
    label: "权限组",
    icon: <ShieldCheck />,
    permissions: ["admin.permission_groups.view"],
  },
  {
    key: "domains",
    label: "域名",
    icon: <Globe2 />,
    permissions: ["admin.domains.view", "admin.dns.view"],
  },
  { key: "mailboxes", label: "邮箱账号", icon: <Mailbox />, permissions: ["admin.mailboxes.view"] },
  { key: "aliases", label: "别名转发", icon: <Copy />, permissions: ["admin.aliases.view"] },
  { key: "messages", label: "全部邮件", icon: <Inbox />, permissions: ["admin.messages.view"] },
  {
    key: "settings",
    label: "系统设置",
    icon: <Settings />,
    permissions: ["admin.settings.view", "admin.templates.view"],
  },
]

export function ProtectedLayout() {
  return (
    <AuthGuard>
      <ProtectedContent />
    </AuthGuard>
  )
}

function ProtectedContent() {
  const me = useMe()
  const location = useLocation()
  const logout = useLogout()

  const user = me.data!.user
  const pathname = location.pathname.toLowerCase()
  const isMailRoute = pathname === "/" || pathname.startsWith("/mail")
  const isProfileRoute = pathname.startsWith("/profile")
  const isAdminRoute = pathname.startsWith("/admin")
  const adminSection = new URLSearchParams(location.search).get("section") || "overview"
  const visibleAdminSections = adminSections.filter((item) =>
    hasAnyPermission(user, item.permissions)
  )

  if (isMailRoute || isProfileRoute) {
    return <Outlet />
  }

  return (
    <SidebarProvider>
      <Sidebar collapsible="icon">
        <SidebarHeader>
          <SidebarMenu>
            <SidebarMenuItem>
              <SidebarMenuButton size="lg" asChild>
                <Link to="/">
                  <div className="flex aspect-square size-8 items-center justify-center rounded-lg bg-primary text-primary-foreground">
                    <Mail className="size-4" />
                  </div>
                  <div className="grid flex-1 text-left text-sm leading-tight">
                    <span className="truncate font-semibold">LanQin Email</span>
                  </div>
                </Link>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarHeader>
        <SidebarContent>
          {isAdminRoute && visibleAdminSections.length > 0 && (
            <SidebarGroup>
              <SidebarGroupContent>
                <SidebarMenu>
                  <AdminSectionItems activeSection={adminSection} sections={visibleAdminSections} />
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          )}
        </SidebarContent>
        <SidebarFooter>
          <SidebarMenu>
            <SidebarMenuItem>
              <SidebarMenuButton size="lg" className="group-data-[collapsible=icon]:!p-0" asChild>
                <Link to="/profile">
                  <Avatar className="h-8 w-8 rounded-lg">
                    <AvatarFallback className="rounded-lg bg-muted text-foreground">
                      {user.displayName.slice(0, 1).toUpperCase()}
                    </AvatarFallback>
                  </Avatar>
                  <div className="grid flex-1 text-left text-sm leading-tight">
                    <span className="truncate font-semibold">{user.displayName}</span>
                    <span className="truncate text-xs text-muted-foreground">{user.email}</span>
                  </div>
                  <Badge
                    variant={user.role === "admin" ? "default" : "secondary"}
                    className="ml-auto text-[10px]"
                  >
                    {user.role === "admin" ? "超级管理员" : "普通用户"}
                  </Badge>
                </Link>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
          <div className="p-2">
            <Button variant="outline" size="sm" className="w-full gap-2 text-xs" onClick={logout}>
              <LogOut className="h-3.5 w-3.5" />
              退出登录
            </Button>
          </div>
        </SidebarFooter>
        <SidebarRail />
      </Sidebar>
      <SidebarInset>
        <div className="flex min-h-svh flex-col bg-muted/20">
          <div className="flex h-12 items-center gap-3 border-b bg-background px-3 md:hidden">
            <SidebarTrigger aria-label="打开导航" />
            <div className="min-w-0 flex-1 truncate text-sm font-semibold">
              {isAdminRoute
                ? visibleAdminSections.find((item) => item.key === adminSection)?.label ||
                  "系统管理"
                : "LanQin Email"}
            </div>
          </div>
          <Outlet />
        </div>
      </SidebarInset>
    </SidebarProvider>
  )
}

function AdminSectionItems({
  activeSection,
  sections,
}: {
  activeSection: string
  sections: typeof adminSections
}) {
  const { isMobile, setOpenMobile } = useSidebar()

  function closeMobile() {
    if (isMobile) setOpenMobile(false)
  }
  return sections.map((item) => (
    <SidebarMenuItem key={item.key}>
      <SidebarMenuButton asChild isActive={activeSection === item.key} tooltip={item.label}>
        <Link to={`/admin?section=${item.key}`} onClick={closeMobile}>
          {item.icon}
          <span>{item.label}</span>
        </Link>
      </SidebarMenuButton>
    </SidebarMenuItem>
  ))
}
