import { Link } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { ScrollArea } from "@/components/ui/scroll-area"
import type { AdminOverviewSlots, AdminShellSlots } from "./contract"

export function WorkbenchShell({
  brand,
  brandLabel,
  compactLanguage,
  account,
  accountLabel,
  signOut,
  navigationLabel,
  activeKey,
  links,
  children,
}: AdminShellSlots) {
  return (
    <div data-lanqin-admin-layout="workbench" className="flex h-svh min-w-0 flex-col bg-muted/20">
      <header className="shrink-0 border-b bg-background">
        <div className="mx-auto flex min-h-14 max-w-[100rem] items-center justify-between gap-2 px-3 sm:gap-4 sm:px-6">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="min-w-0 flex-1 justify-start gap-2 overflow-hidden px-1"
            asChild
          >
            <Link to="/" aria-label={brandLabel}>
              {brand}
            </Link>
          </Button>
          <div className="flex min-w-0 shrink-0 items-center gap-1 max-sm:[&_[data-admin-shell-detail]]:hidden sm:gap-2">
            {compactLanguage}
            <Button type="button" variant="ghost" size="sm" className="min-w-0 gap-2 px-1" asChild>
              <Link to="/profile" aria-label={accountLabel}>
                {account}
              </Link>
            </Button>
            {signOut}
          </div>
        </div>
        {links.length > 0 && (
          <nav
            aria-label={navigationLabel}
            className="mx-auto flex max-w-[100rem] items-center gap-1 overflow-x-auto px-3 pb-2 pt-1.5 sm:px-6"
          >
            {links.map(({ key, href, label, icon }) => (
              <Button
                key={key}
                type="button"
                variant={activeKey === key ? "secondary" : "ghost"}
                size="sm"
                className="shrink-0 gap-2"
                asChild
              >
                <Link to={href} aria-current={activeKey === key ? "page" : undefined}>
                  {icon}
                  {label}
                </Link>
              </Button>
            ))}
          </nav>
        )}
      </header>
      <ScrollArea className="min-h-0 flex-1">
        <div className="mx-auto w-full max-w-[100rem]">{children}</div>
      </ScrollArea>
    </div>
  )
}

export function WorkbenchOverview({
  metrics,
  status,
  setup,
  domains,
  runtime,
}: AdminOverviewSlots) {
  return (
    <div className="space-y-6">
      {metrics && (
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">{metrics}</div>
      )}
      <div className="grid items-start gap-6 lg:grid-cols-[minmax(15rem,1fr)_minmax(0,2fr)]">
        <div className="min-w-0 space-y-6">
          {setup}
          {runtime}
        </div>
        <div className="min-w-0 space-y-6">
          {status}
          {domains}
        </div>
      </div>
    </div>
  )
}
