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
import { api, type PublicSettings } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { adminThemeKey } from "@/lib/site-theme"
import { DEFAULT_THEME_ID, resolveThemeId, themes } from "@/themes/registry"

const publicSettingsKey = ["public-settings"] as const

export function ThemeSettingsCard({ canUpdate }: { canUpdate: boolean }) {
  useLanguage()
  const client = useQueryClient()
  const current = useQuery({ queryKey: adminThemeKey, queryFn: api.uiTheme })
  const [selection, setSelection] = React.useState<string>()
  const update = useMutation({
    mutationFn: api.setUITheme,
    onSuccess: (theme) => {
      client.setQueryData(adminThemeKey, theme)
      client.setQueryData<PublicSettings>(publicSettingsKey, (settings) =>
        settings ? { ...settings, themeId: theme.themeId } : undefined
      )
      void client.invalidateQueries({ queryKey: publicSettingsKey })
      setSelection(undefined)
    },
  })

  const savedTheme = current.data?.themeId
  const active = resolveThemeId(savedTheme, themes)
  const selected = selection ?? active
  const disabled = !canUpdate || !current.isSuccess || update.isPending

  function selectTheme(id: string) {
    update.reset()
    setSelection(id)
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{uiText("外观主题")}</CardTitle>
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
        {current.isSuccess && (
          <>
            {savedTheme !== active && (
              <p role="status">{uiText("当前主题未安装，已回退到经典主题")}</p>
            )}
            <div className="space-y-2">
              <Label htmlFor="site-theme-select">{uiText("全站主题")}</Label>
              <Select value={selected} onValueChange={selectTheme} disabled={disabled}>
                <SelectTrigger id="site-theme-select" className="max-w-sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {themes.map((theme) => (
                    <SelectItem key={theme.id} value={theme.id}>
                      <span data-lanqin-i18n-ignore>{uiText(theme.name)}</span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {canUpdate ? (
              <div className="flex flex-wrap gap-2">
                <Button
                  type="button"
                  disabled={disabled || selected === savedTheme}
                  onClick={() => update.mutate(selected)}
                >
                  {update.isPending ? uiText("保存中...") : uiText("应用主题")}
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  disabled={disabled || savedTheme === DEFAULT_THEME_ID}
                  onClick={() => update.mutate(DEFAULT_THEME_ID)}
                >
                  {uiText("关闭自定义主题")}
                </Button>
              </div>
            ) : (
              <p>{uiText("你没有修改主题的权限")}</p>
            )}
            {update.isSuccess && <p role="status">{uiText("主题设置已保存")}</p>}
          </>
        )}
        {update.isError && <p role="alert">{uiText("无法保存主题设置")}</p>}
      </CardContent>
    </Card>
  )
}
