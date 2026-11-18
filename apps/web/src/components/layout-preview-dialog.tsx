import * as React from "react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import type { ThemePackageAppearance } from "@/lib/api-types"
import { themePackageVariables } from "@/themes/runtime-package"
import { uiText, useLanguage } from "@/lib/language"
import { CLASSIC_LAYOUT, layoutCatalog, type LayoutSurface } from "@/themes/layouts/catalog"

type Props = {
  surface: LayoutSurface
  id: string
  packagePreview?: {
    id: string
    name: string
    appearance: ThemePackageAppearance
    focusReturn?: HTMLElement
  }
  onClose: () => void
}
const pages = {
  front: [
    { id: "mail", name: "邮箱" },
    { id: "login", name: "登录" },
    { id: "profile", name: "个人中心" },
  ],
  admin: [
    { id: "overview", name: "概览" },
    { id: "settings", name: "系统设置" },
  ],
} as const

export function LayoutPreviewDialog({ surface, id, packagePreview, onClose }: Props) {
  useLanguage()
  const [mobile, setMobile] = React.useState(false)
  const [dark, setDark] = React.useState(false)
  const [page, setPage] = React.useState<string>(pages[surface][0].id)
  const workbench = id !== CLASSIC_LAYOUT
  const name = layoutCatalog.find((item) => item.id === id)?.name ?? "经典主题"
  const previewVariables = packagePreview
    ? ({
        "--background": dark ? "240 10% 3.9%" : "0 0% 100%",
        "--foreground": dark ? "0 0% 98%" : "240 10% 3.9%",
        "--card": dark ? "240 3.7% 15.9%" : "240 4.8% 95.9%",
        "--card-foreground": dark ? "0 0% 98%" : "240 10% 3.9%",
        "--border": dark ? "240 3.7% 15.9%" : "240 5.9% 90%",
        "--radius": "0.5rem",
        ...themePackageVariables(packagePreview.appearance, dark ? "dark" : "light"),
      } as React.CSSProperties)
    : undefined
  const frame = previewVariables
    ? "border-border bg-background text-foreground"
    : dark
      ? "border-zinc-700 bg-zinc-900 text-zinc-100"
      : "border-zinc-300 bg-white text-zinc-900"
  const panel = previewVariables
    ? "border-border bg-card text-card-foreground"
    : dark
      ? "border-zinc-700 bg-zinc-800"
      : "border-zinc-200 bg-zinc-50"
  const tabs =
    surface === "front"
      ? ["收件箱", "已发送", "草稿箱", "归档"]
      : ["概览", "用户", "域名", "系统设置"]

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent
        aria-describedby={undefined}
        onCloseAutoFocus={(event) => {
          const target = packagePreview?.focusReturn
          if (!target?.isConnected) return
          event.preventDefault()
          target.focus()
        }}
        className="max-h-[90dvh] max-w-[min(95vw,58rem)] overflow-auto"
      >
        <DialogHeader>
          <DialogTitle data-lanqin-i18n-ignore>
            {uiText("预览模板")} · {packagePreview?.name ?? uiText(name)}
          </DialogTitle>
        </DialogHeader>
        <div className="flex flex-wrap gap-2">
          {pages[surface].map((item) => (
            <Button
              type="button"
              size="sm"
              variant={page === item.id ? "secondary" : "ghost"}
              aria-pressed={page === item.id}
              onClick={() => setPage(item.id)}
              key={item.id}
            >
              {uiText(item.name)}
            </Button>
          ))}
          <Button
            size="sm"
            type="button"
            variant="outline"
            aria-pressed={mobile}
            onClick={() => setMobile(!mobile)}
          >
            {uiText(mobile ? "桌面尺寸" : "手机尺寸")}
          </Button>
          <Button
            size="sm"
            type="button"
            variant="outline"
            aria-pressed={dark}
            onClick={() => setDark(!dark)}
          >
            {uiText(dark ? "浅色预览" : "深色预览")}
          </Button>
        </div>
        <p className="text-sm text-muted-foreground">{uiText("预览使用示例数据，不会应用模板")}</p>
        <div
          className={`mx-auto w-full overflow-hidden rounded-lg border text-sm ${frame} ${mobile ? "max-w-xs" : ""}`}
          style={previewVariables}
          data-package-preview={packagePreview?.id}
          data-layout-preview={id}
          data-lanqin-i18n-ignore
        >
          <div className={`flex flex-wrap items-center gap-3 border-b p-3 ${panel}`}>
            <strong>LanQin Email</strong>
            {workbench && !mobile && page !== "login" && (
              <div className="flex flex-wrap gap-3">
                {tabs.map((tab) => (
                  <span key={tab}>{uiText(tab)}</span>
                ))}
              </div>
            )}
          </div>
          {page === "login" || page === "profile" ? (
            <div
              className={`flex min-h-48 items-center gap-4 p-4 ${workbench && !mobile ? "" : "justify-center"}`}
            >
              {workbench && !mobile && <strong className="flex-1 p-4">LanQin Email</strong>}
              <div className={`w-full max-w-xs space-y-3 rounded border p-4 ${panel}`}>
                <strong>{uiText(page === "login" ? "登录" : "个人中心")}</strong>
                <div className="h-7 rounded border" />
                <div className="h-7 rounded border" />
              </div>
            </div>
          ) : (
            <div className="flex min-h-48">
              {!workbench && !mobile && (
                <div className={`w-36 space-y-3 border-r p-3 ${panel}`}>
                  {tabs.map((tab) => (
                    <div key={tab}>{uiText(tab)}</div>
                  ))}
                </div>
              )}
              {surface === "front" ? (
                <div
                  className={`grid flex-1 gap-3 p-3 ${mobile ? "" : "grid-cols-[minmax(8rem,1fr)_minmax(10rem,2fr)]"}`}
                >
                  <div className={`space-y-3 rounded border p-3 ${panel}`}>
                    <strong>{uiText("收件箱")}</strong>
                    <div>{uiText("示例邮件")}</div>
                    <div className="border-t pt-2">LanQin Email</div>
                  </div>
                  {!mobile && (
                    <div className={`space-y-3 rounded border p-3 ${panel}`}>
                      <strong>{uiText("示例邮件")}</strong>
                      <div>LanQin Email</div>
                    </div>
                  )}
                </div>
              ) : (
                <div className="flex-1 space-y-3 p-3">
                  <strong>{uiText(page === "overview" ? "概览" : "系统设置")}</strong>
                  <div
                    className={`grid gap-2 ${mobile ? "" : workbench ? "grid-cols-3" : "grid-cols-2"}`}
                  >
                    {["用户", "域名", "邮箱"].map((tab, index) => (
                      <div className={`space-y-1 rounded border p-3 ${panel}`} key={tab}>
                        <div>{uiText(tab)}</div>
                        <strong>{[12, 3, 24][index]}</strong>
                      </div>
                    ))}
                  </div>
                </div>
              )}
            </div>
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
