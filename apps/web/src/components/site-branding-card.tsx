import * as React from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { Image, Trash2, Upload } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useToast } from "@/hooks/use-toast"
import { api, type PublicSettings, type SystemSettings } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"

const publicSettingsKey = ["public-settings"] as const
const systemSettingsKey = ["admin", "settings"] as const
const faviconPath = "/api/favicon.ico"

function getSiteIcon() {
  return document.querySelector<HTMLLinkElement>('link[rel="icon"]')
}

export function SiteBrandingCard({
  title,
  canUpdate,
}: {
  title: string | undefined
  canUpdate: boolean
}) {
  useLanguage()
  const client = useQueryClient()
  const { toast } = useToast()
  const [titleDraft, setTitleDraft] = React.useState<string>()
  const [faviconHref, setFaviconHref] = React.useState(
    () => getSiteIcon()?.getAttribute("href") || faviconPath
  )
  const [unavailableHref, setUnavailableHref] = React.useState<string>()
  const faviconInput = React.useRef<HTMLInputElement>(null)

  const saveTitle = useMutation({
    mutationFn: api.updateSiteBranding,
    onSuccess: async ({ siteTitle }) => {
      await client.cancelQueries({ queryKey: systemSettingsKey, exact: true })
      client.setQueryData<SystemSettings>(systemSettingsKey, (settings) =>
        settings ? { ...settings, siteTitle } : undefined
      )
      client.setQueryData<PublicSettings>(publicSettingsKey, (settings) =>
        settings ? { ...settings, siteTitle } : undefined
      )
      void client.invalidateQueries({ queryKey: publicSettingsKey, exact: true })
      setTitleDraft(undefined)
      toast({ title: "网站标题已保存" })
    },
    onError: (error) => toast({ title: "保存失败", description: errorMessage(error) }),
  })

  function refreshFavicon() {
    const href = `${faviconPath}?v=${Date.now()}`
    const icon = getSiteIcon()
    if (icon) icon.href = href
    setFaviconHref(href)
    setUnavailableHref(undefined)
  }

  const faviconUpload = useMutation({
    mutationFn: api.uploadFavicon,
    onSuccess: () => {
      refreshFavicon()
      toast({ title: "网站图标已更新" })
    },
    onError: (error) => toast({ title: "图标上传失败", description: errorMessage(error) }),
  })
  const faviconDelete = useMutation({
    mutationFn: api.deleteFavicon,
    onSuccess: () => {
      refreshFavicon()
      toast({ title: "网站图标已移除" })
    },
    onError: (error) => toast({ title: "移除图标失败", description: errorMessage(error) }),
  })

  const displayedTitle = titleDraft ?? title ?? ""
  const newTitle = displayedTitle.trim()
  const titleDisabled = !canUpdate || title === undefined || saveTitle.isPending
  const faviconDisabled = !canUpdate || faviconUpload.isPending || faviconDelete.isPending

  return (
    <Card>
      <CardHeader>
        <CardTitle>{uiText("站点品牌")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-5">
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(event) => {
            event.preventDefault()
            if (!titleDisabled && newTitle && newTitle !== title) saveTitle.mutate(newTitle)
          }}
        >
          <div className="w-full max-w-sm space-y-2">
            <Label htmlFor="site-branding-title">{uiText("网站标题")}</Label>
            <Input
              id="site-branding-title"
              value={displayedTitle}
              required
              maxLength={80}
              readOnly={!canUpdate}
              disabled={title === undefined || saveTitle.isPending}
              onChange={(event) => setTitleDraft(event.currentTarget.value)}
            />
          </div>
          <Button type="submit" disabled={titleDisabled || !newTitle || newTitle === title}>
            {saveTitle.isPending ? uiText("保存中...") : uiText("保存网站标题")}
          </Button>
        </form>

        <div className="space-y-2">
          <p id="site-branding-icon-label" className="text-sm font-medium">
            {uiText("网站图标")}
          </p>
          <div
            className="flex flex-wrap items-center gap-2"
            role="group"
            aria-labelledby="site-branding-icon-label"
          >
            <div className="flex size-9 items-center justify-center rounded-md border bg-muted">
              {unavailableHref === faviconHref ? (
                <Image className="size-4 text-muted-foreground" aria-hidden />
              ) : (
                <img
                  key={faviconHref}
                  src={faviconHref}
                  alt={uiText("网站图标")}
                  className="size-5 object-contain"
                  onError={() => setUnavailableHref(faviconHref)}
                />
              )}
            </div>
            <Input
              ref={faviconInput}
              type="file"
              accept=".ico,image/x-icon,image/vnd.microsoft.icon"
              className="hidden"
              aria-label={uiText("选择 ICO 文件")}
              disabled={faviconDisabled}
              onChange={(event) => {
                const file = event.currentTarget.files?.[0]
                if (file && !faviconDisabled) faviconUpload.mutate(file)
                event.currentTarget.value = ""
              }}
            />
            <Button
              type="button"
              variant="outline"
              disabled={faviconDisabled}
              onClick={() => faviconInput.current?.click()}
            >
              <Upload className="size-4" aria-hidden />
              {uiText(faviconUpload.isPending ? "正在上传..." : "上传 ICO")}
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              aria-label={uiText("移除网站图标")}
              title={uiText("移除网站图标")}
              disabled={faviconDisabled}
              onClick={() => faviconDelete.mutate()}
            >
              <Trash2 className="size-4" aria-hidden />
            </Button>
          </div>
        </div>

        {!canUpdate && <p>{uiText("无法修改站点品牌")}</p>}
      </CardContent>
    </Card>
  )
}
