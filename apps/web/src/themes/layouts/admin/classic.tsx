import { Link } from "react-router-dom"
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
import { ScrollArea } from "@/components/ui/scroll-area"
import type { AdminOverviewSlots, AdminShellSlots } from "./contract"

export function ClassicShell({
  brand,
  brandLabel,
  sidebarLanguage,
  account,
  accountLabel,
  signOut,
  navigationLabel,
  openNavigationLabel,
  currentLabel,
  activeKey,
  links,
  children,
}: AdminShellSlots) {
  return (
    <SidebarProvider data-lanqin-admin-layout="classic">
      <Sidebar collapsible="icon">
        <SidebarHeader>
          <SidebarMenu>
            <SidebarMenuItem>
              <SidebarMenuButton size="lg" asChild>
                <Link to="/" aria-label={brandLabel}>
                  {brand}
                </Link>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarHeader>
        <SidebarContent>
          {links.length > 0 && (
            <SidebarGroup>
              <SidebarGroupContent>
                <nav aria-label={navigationLabel}>
                  <SidebarMenu>
                    <ClassicSectionLinks activeKey={activeKey} links={links} />
                  </SidebarMenu>
                </nav>
              </SidebarGroupContent>
            </SidebarGroup>
          )}
        </SidebarContent>
        <SidebarFooter>
          <SidebarMenu>
            <SidebarMenuItem>{sidebarLanguage}</SidebarMenuItem>
            <SidebarMenuItem>
              <SidebarMenuButton size="lg" className="group-data-[collapsible=icon]:!p-0" asChild>
                <Link to="/profile" aria-label={accountLabel}>
                  {account}
                </Link>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
          <div className="p-2">{signOut}</div>
        </SidebarFooter>
        <SidebarRail />
      </Sidebar>
      <SidebarInset>
        <div className="flex min-h-svh min-w-0 flex-col bg-muted/20">
          <div className="flex h-12 items-center gap-3 border-b bg-background px-3 md:hidden">
            <SidebarTrigger aria-label={openNavigationLabel} />
            <div className="min-w-0 flex-1 truncate text-sm font-semibold">{currentLabel}</div>
          </div>
          <ScrollArea className="h-[calc(100svh-3rem)] md:h-svh">{children}</ScrollArea>
        </div>
      </SidebarInset>
    </SidebarProvider>
  )
}

function ClassicSectionLinks({ activeKey, links }: Pick<AdminShellSlots, "activeKey" | "links">) {
  const { isMobile, setOpenMobile } = useSidebar()
  return links.map(({ key, href, label, icon }) => (
    <SidebarMenuItem key={key}>
      <SidebarMenuButton asChild isActive={activeKey === key} tooltip={label}>
        <Link
          to={href}
          aria-current={activeKey === key ? "page" : undefined}
          onClick={() => {
            if (isMobile) setOpenMobile(false)
          }}
        >
          {icon}
          <span>{label}</span>
        </Link>
      </SidebarMenuButton>
    </SidebarMenuItem>
  ))
}

export function ClassicOverview({ metrics, status, setup, domains, runtime }: AdminOverviewSlots) {
  return (
    <div className="space-y-6">
      {metrics && <div className="grid gap-4 md:grid-cols-4">{metrics}</div>}
      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_420px]">
        {status}
        {setup}
      </div>
      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_420px]">
        {domains}
        {runtime}
      </div>
    </div>
  )
}
