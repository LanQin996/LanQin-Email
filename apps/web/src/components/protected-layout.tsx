import * as React from "react"
import { Outlet, Link, useLocation } from "react-router-dom"
import { BarChart3, Copy, Globe2, Inbox, LogOut, Mail, Mailbox, Settings, Users } from "lucide-react"
import { useMe } from "@/hooks/use-me"
import { useLogout } from "@/hooks/use-logout"
import { AuthGuard } from "@/components/auth-guard"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
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

const adminSections = [
  { key: "overview", label: "概览", icon: <BarChart3 /> },
  { key: "users", label: "用户", icon: <Users /> },
  { key: "domains", label: "域名", icon: <Globe2 /> },
  { key: "mailboxes", label: "邮箱账号", icon: <Mailbox /> },
  { key: "aliases", label: "别名转发", icon: <Copy /> },
  { key: "messages", label: "全部邮件", icon: <Inbox /> },
  { key: "settings", label: "系统设置", icon: <Settings /> },
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
  const isMailRoute = location.pathname === "/" || location.pathname.startsWith("/mail")
  const isProfileRoute = location.pathname.startsWith("/profile")
  const isAdminRoute = location.pathname.startsWith("/admin")
  const adminSection = new URLSearchParams(location.search).get("section") || "overview"

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
          {user.role === "admin" && isAdminRoute && (
            <SidebarGroup>
              <SidebarGroupContent>
                <SidebarMenu>
                  <AdminSectionItems activeSection={adminSection} />
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
                  <Badge variant={user.role === "admin" ? "default" : "secondary"} className="ml-auto text-[10px]">
                    {user.role === "admin" ? "管理员" : "用户"}
                  </Badge>
                </Link>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
          <div className="p-2">
            <Button variant="outline" size="sm" className="w-full gap-2 text-xs" onClick={logout}>
              <LogOut className="h-3.5 w-3.5" />退出登录
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
              {isAdminRoute ? adminSections.find((item) => item.key === adminSection)?.label || "系统管理" : "LanQin Email"}
            </div>
          </div>
          <Outlet />
        </div>
      </SidebarInset>
    </SidebarProvider>
  )
}

function AdminSectionItems({ activeSection }: { activeSection: string }) {
  const { isMobile, setOpenMobile } = useSidebar()

  function closeMobile() {
    if (isMobile) setOpenMobile(false)
  }
  return adminSections.map((item) => (
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
