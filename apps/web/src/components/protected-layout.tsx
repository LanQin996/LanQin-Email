import * as React from "react"
import { Navigate, Outlet, Link, useLocation, useNavigate } from "react-router-dom"
import { BarChart3, Copy, Globe2, Inbox, LogOut, Mail, Mailbox, Settings, Users } from "lucide-react"
import { useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { useMe } from "@/hooks/use-me"
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
  const me = useMe()
  const location = useLocation()
  const navigate = useNavigate()
  const qc = useQueryClient()

  if (me.isLoading) return <AuthLoading />
  if (me.isError && me.error.message.includes("请求超时")) return <AuthError message={me.error.message} onRetry={() => me.refetch()} />
  if (me.isError || !me.data?.user) return <Navigate to="/login" replace state={{ from: location.pathname }} />

  const user = me.data.user
  const isMailRoute = location.pathname === "/" || location.pathname.startsWith("/mail")
  const isProfileRoute = location.pathname.startsWith("/profile")
  const isAdminRoute = location.pathname.startsWith("/admin")
  const adminSection = new URLSearchParams(location.search).get("section") || "overview"

  async function logout() {
    await api.logout().catch(() => undefined)
    qc.clear()
    navigate("/login", { replace: true })
  }

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
                  {adminSections.map((item) => (
                    <SidebarMenuItem key={item.key}>
                      <SidebarMenuButton asChild isActive={adminSection === item.key} tooltip={item.label}>
                        <Link to={`/admin?section=${item.key}`}>
                          {item.icon}
                          <span>{item.label}</span>
                        </Link>
                      </SidebarMenuButton>
                    </SidebarMenuItem>
                  ))}
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
          <div className="flex h-12 items-center border-b bg-white px-3 md:hidden">
            <SidebarTrigger />
          </div>
          <Outlet />
        </div>
      </SidebarInset>
    </SidebarProvider>
  )
}

function AuthLoading() {
  return <div className="grid min-h-screen place-items-center text-muted-foreground">加载中...</div>
}

function AuthError({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <div className="grid min-h-screen place-items-center bg-background px-4">
      <div className="w-full max-w-sm space-y-4 text-center">
        <div className="text-sm font-medium">无法连接后端服务</div>
        <div className="text-sm text-muted-foreground">{message}</div>
        <Button type="button" variant="outline" onClick={onRetry}>重新加载</Button>
      </div>
    </div>
  )
}
