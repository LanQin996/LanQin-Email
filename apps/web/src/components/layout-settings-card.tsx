import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { api, type PublicSettings, type UILayoutSettings } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import {
  activeAdminLayoutKey,
  managedLayoutsKey,
  publicLayoutKey,
  useTemplateLayout,
} from "@/lib/template-layout"
import {
  CLASSIC_LAYOUT,
  layoutCatalog,
  resolveLayout,
  supportsLayout,
  type LayoutSurface,
} from "@/themes/layouts/catalog"
import { LayoutPreviewDialog } from "./layout-preview-dialog"

const labels = {
  front: { key: "frontLayoutId", label: "用户前台模板" },
  admin: { key: "adminLayoutId", label: "管理后台模板" },
} as const

export function LayoutSettingsCard({ canUpdate }: { canUpdate: boolean }) {
  useLanguage()
  const displayedAdminLayout = useTemplateLayout("admin")
  const client = useQueryClient()
  const current = useQuery({
    queryKey: managedLayoutsKey,
    queryFn: api.uiLayouts,
    refetchOnWindowFocus: true,
    refetchInterval: 60_000,
  })
  const [selection, setSelection] = React.useState<Partial<UILayoutSettings>>({})
  const [preview, setPreview] = React.useState<{ surface: LayoutSurface; id: string }>()
  const update = useMutation({
    mutationFn: api.setUILayouts,
    onSuccess: async (saved, submitted) => {
      await Promise.all([
        client.cancelQueries({ queryKey: managedLayoutsKey }),
        client.cancelQueries({ queryKey: publicLayoutKey }),
        client.cancelQueries({ queryKey: activeAdminLayoutKey }),
      ])
      client.setQueryData(managedLayoutsKey, saved)
      client.setQueryData<PublicSettings>(publicLayoutKey, (settings) =>
        settings ? { ...settings, frontLayoutId: saved.frontLayoutId } : undefined
      )
      client.setQueryData(activeAdminLayoutKey, { adminLayoutId: saved.adminLayoutId })
      setSelection((previous) => ({
        frontLayoutId: submitted.frontLayoutId ? undefined : previous.frontLayoutId,
        adminLayoutId: submitted.adminLayoutId ? undefined : previous.adminLayoutId,
      }))
      void client.invalidateQueries({ queryKey: publicLayoutKey })
      void client.invalidateQueries({ queryKey: activeAdminLayoutKey })
    },
  })

  const chosen = (surface: LayoutSurface) =>
    selection[labels[surface].key] ?? resolveLayout(current.data?.[labels[surface].key], surface)
  const pair = chosen("front")
  const canApplyPair =
    pair === chosen("admin") &&
    supportsLayout(pair, "front") &&
    supportsLayout(pair, "admin") &&
    (current.data?.frontLayoutId !== pair || current.data.adminLayoutId !== pair)

  return (
    <Card>
      <CardHeader>
        <CardTitle>{uiText("界面模板管理")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {current.isPending && <p role="status">{uiText("正在加载主题设置")}</p>}
        {current.isError && (
          <div className="space-y-2">
            <p role="alert">{uiText("无法加载主题设置")}</p>
            <Button type="button" variant="outline" onClick={() => void current.refetch()}>
              {uiText("重试")}
            </Button>
          </div>
        )}
        {current.data && (
          <>
            <div className="grid gap-4 xl:grid-cols-2">
              {(["front", "admin"] as const).map((surface) => {
                const { key, label } = labels[surface]
                const saved = current.data?.[key]
                const active = resolveLayout(saved, surface)
                const options = layoutCatalog.filter((item) => item.surfaces.includes(surface))
                const id = chosen(surface)
                return (
                  <section className="space-y-3 rounded-md border p-4" key={surface}>
                    <Label htmlFor={key}>{uiText(label)}</Label>
                    <p className="text-sm" aria-live="polite" data-lanqin-i18n-ignore>
                      {uiText("当前")}：{uiText(options.find((item) => item.id === active)!.name)}
                    </p>
                    {saved !== active && (
                      <p role="status">{uiText("当前主题未安装，已回退到经典主题")}</p>
                    )}
                    <Select
                      value={id}
                      disabled={update.isPending}
                      onValueChange={(next) => {
                        update.reset()
                        setSelection((previous) => ({ ...previous, [key]: next }))
                      }}
                    >
                      <SelectTrigger id={key}>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {options.map((item) => (
                          <SelectItem key={item.id} value={item.id}>
                            <span data-lanqin-i18n-ignore>{uiText(item.name)}</span>
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <div className="flex flex-wrap gap-2">
                      <Button
                        type="button"
                        variant="outline"
                        onClick={() => setPreview({ surface, id })}
                      >
                        {uiText("预览模板")}
                      </Button>
                      {canUpdate && (
                        <>
                          <Button
                            type="button"
                            disabled={update.isPending || id === saved}
                            onClick={() => update.mutate({ [key]: id })}
                          >
                            {uiText("应用模板")}
                          </Button>
                          <Button
                            type="button"
                            variant="outline"
                            disabled={update.isPending || saved === CLASSIC_LAYOUT}
                            onClick={() => update.mutate({ [key]: CLASSIC_LAYOUT })}
                          >
                            {uiText("恢复经典模板")}
                          </Button>
                        </>
                      )}
                    </div>
                  </section>
                )
              })}
            </div>
            {canUpdate ? (
              <Button
                type="button"
                variant="outline"
                disabled={update.isPending || !canApplyPair}
                onClick={() => update.mutate({ frontLayoutId: pair, adminLayoutId: pair })}
              >
                {uiText("应用到前台和后台")}
              </Button>
            ) : (
              <p>{uiText("你没有修改主题的权限")}</p>
            )}
            {update.isPending && <p role="status">{uiText("保存中...")}</p>}
            {update.isSuccess && <p role="status">{uiText("主题设置已保存")}</p>}
            {displayedAdminLayout !== resolveLayout(current.data.adminLayoutId, "admin") && (
              <div className="space-y-2">
                <p role="status">{uiText("后台模板已保存，新开的管理页面会使用它")}</p>
                <Button type="button" variant="outline" asChild>
                  <a href="/admin?section=settings&tab=appearance" target="_blank" rel="noopener">
                    {uiText("在新标签页打开已保存的布局")}
                  </a>
                </Button>
              </div>
            )}
          </>
        )}
        {update.isError && <p role="alert">{uiText("无法保存主题设置")}</p>}
        <Button asChild variant="link" className="h-auto px-0">
          <a href="/admin?section=settings&tab=appearance&layout=classic">
            {uiText("以经典布局打开模板设置")}
          </a>
        </Button>
        {preview && <LayoutPreviewDialog {...preview} onClose={() => setPreview(undefined)} />}
      </CardContent>
    </Card>
  )
}
