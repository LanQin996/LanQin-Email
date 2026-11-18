import type { ReactNode, RefObject } from "react"
import type { ImperativePanelHandle } from "react-resizable-panels"
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable"
import { Sidebar } from "@/components/ui/sidebar"
import { cn } from "@/lib/utils"

export type FrontDesktopSlots = {
  /** Existing interactive folder, label, mailbox, or profile navigation. */
  navigation: ReactNode
  /** Complete classic sidebar, including the rail-specific controls. */
  sidebar: ReactNode
  /** Account and mailbox controls for the Workbench top bar. */
  identity?: ReactNode
  primaryNavigation?: ReactNode
  /** Existing actions and search, independent of layout. */
  toolbar: ReactNode
  search?: ReactNode
  /** Already-authorized mail or profile content. Never place data fetching here. */
  content: ReactNode
  sidebarPanelRef: RefObject<ImperativePanelHandle>
  onSidebarToggle: (collapsed: boolean) => void
}

export function ClassicFrontDesktop({
  sidebar,
  toolbar,
  search,
  content,
  sidebarPanelRef,
  onSidebarToggle,
}: FrontDesktopSlots) {
  return (
    <ResizablePanelGroup
      direction="horizontal"
      className="h-full min-h-0 w-full"
      data-front-layout="classic"
    >
      <ResizablePanel
        ref={sidebarPanelRef}
        collapsible
        collapsedSize={4}
        defaultSize={15}
        minSize={11}
        maxSize={24}
        onCollapse={() => onSidebarToggle(true)}
        onExpand={() => onSidebarToggle(false)}
      >
        {sidebar}
      </ResizablePanel>
      <ResizableHandle withHandle />
      <ResizablePanel defaultSize={85} minSize={60}>
        <section className="flex h-full min-h-0 flex-col">
          <header className="flex h-16 shrink-0 items-center justify-between gap-3 border-b px-5">
            {toolbar}
            {search}
          </header>
          {content}
        </section>
      </ResizablePanel>
    </ResizablePanelGroup>
  )
}

export function WorkbenchFrontDesktop({
  navigation,
  identity,
  primaryNavigation,
  toolbar,
  search,
  content,
}: FrontDesktopSlots) {
  return (
    <div className="flex h-full min-h-0 w-full flex-col bg-muted/20" data-front-layout="workbench">
      <header
        className="flex shrink-0 flex-wrap items-center justify-between gap-x-5 gap-y-2 border-b bg-background px-4 py-2.5 lg:px-5"
        data-front-slot="top-bar"
      >
        {identity && <div className="flex min-w-0 flex-wrap items-center gap-2">{identity}</div>}
        {primaryNavigation && (
          <div className="order-last w-full min-w-0 xl:order-none xl:w-auto">
            {primaryNavigation}
          </div>
        )}
        <div className="flex min-w-0 flex-1 flex-wrap items-center justify-end gap-2">
          {toolbar}
          {search && <div className="w-full max-w-72 sm:w-56 lg:w-72">{search}</div>}
        </div>
      </header>
      <div className="grid min-h-0 flex-1 grid-cols-[minmax(9.5rem,16%)_minmax(0,1fr)]">
        <Sidebar collapsible="none" className="min-h-0 w-full border-r bg-sidebar">
          {navigation}
        </Sidebar>
        <section className="flex min-h-0 min-w-0 flex-col p-2 lg:p-3">{content}</section>
      </div>
    </div>
  )
}

export type FrontReaderSlots = {
  list: ReactNode
  reader: ReactNode
}

export function ClassicFrontReader({ list, reader }: FrontReaderSlots) {
  return (
    <ResizablePanelGroup direction="horizontal" className="min-h-0 flex-1">
      <ResizablePanel defaultSize={32} minSize={24} maxSize={44}>
        {list}
      </ResizablePanel>
      <ResizableHandle withHandle />
      <ResizablePanel defaultSize={68} minSize={44}>
        {reader}
      </ResizablePanel>
    </ResizablePanelGroup>
  )
}

export function WorkbenchFrontReader({ list, reader }: FrontReaderSlots) {
  return (
    <div className="grid min-h-0 flex-1 grid-cols-[minmax(11rem,36%)_minmax(0,1fr)] gap-2 lg:gap-3">
      <div
        className="min-h-0 min-w-0 overflow-hidden rounded-lg border bg-card"
        data-front-slot="message-list"
      >
        {list}
      </div>
      <div
        className="min-h-0 min-w-0 overflow-hidden rounded-lg border bg-card"
        data-front-slot="reader"
      >
        {reader}
      </div>
    </div>
  )
}

export type FrontAuthSlots = {
  /** Site identity and existing language control. */
  brand: ReactNode
  /** Login or registration form and its existing navigation/actions. */
  children: ReactNode
  footer?: ReactNode
}

/** Keeps the same form DOM mounted when an administrator changes the layout. */
export function FrontAuthFrame({
  layoutId,
  brand,
  children,
  footer,
}: FrontAuthSlots & { layoutId: string }) {
  const workbench = layoutId === "workbench"
  return (
    <div
      className={cn(
        "min-h-screen bg-muted/20 px-4 py-10",
        !workbench && "flex items-center justify-center"
      )}
      data-front-layout={layoutId}
    >
      <div
        className={cn(
          "w-full max-w-[420px]",
          workbench &&
            "mx-auto grid min-h-[min(36rem,calc(100svh-5rem))] overflow-hidden rounded-lg border bg-background md:max-w-4xl md:grid-cols-[minmax(0,2fr)_minmax(21rem,3fr)]"
        )}
      >
        <header
          className={
            workbench
              ? "border-b bg-muted/40 px-6 py-5 md:flex md:flex-col md:justify-center md:border-b-0 md:border-r md:p-9"
              : "mb-7 text-center"
          }
        >
          {brand}
        </header>
        <div
          className={cn(workbench && "flex min-w-0 flex-col justify-center p-6 sm:p-7 md:px-10")}
        >
          <div className={cn(!workbench && "rounded-lg border bg-background p-6 shadow-sm sm:p-7")}>
            {children}
          </div>
          {footer}
        </div>
      </div>
    </div>
  )
}
