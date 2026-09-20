import { errorMessage } from "@/lib/ui-errors"
import { getInitialLanguage, uiText, useLanguage as useUiLanguage } from "@/lib/language"

import * as React from "react"
import { Bot, RefreshCcw, Trash2 } from "lucide-react"
import type { TelegramSettings } from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { PasswordInput } from "@/components/ui/password-input"
import { Switch } from "@/components/ui/switch"
import { ConfirmDialog } from "@/components/confirm-dialog"

type TelegramSettingsCardProps = {
  item?: TelegramSettings
  pending: boolean
  onSave: (payload: {
    botToken?: string
    chatId: string
    enabled: boolean
  }) => Promise<TelegramSettings>
  onTest: () => Promise<void>
  onDelete: () => Promise<void>
}

export function TelegramSettingsCard({
  item,
  pending,
  onSave,
  onTest,
  onDelete,
}: TelegramSettingsCardProps) {
  useUiLanguage()

  const [botToken, setBotToken] = React.useState("")
  const [chatId, setChatId] = React.useState("")
  const [enabled, setEnabled] = React.useState(true)
  const [confirmOpen, setConfirmOpen] = React.useState(false)

  React.useEffect(() => {
    setBotToken("")
    setChatId(item?.chatId || "")
    setEnabled(item?.configured ? item.enabled : true)
  }, [item?.chatId, item?.configured, item?.enabled])

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    try {
      await onSave({ botToken: botToken.trim() || undefined, chatId: chatId.trim(), enabled })
      setBotToken("")
    } catch {
      // Mutation-level error handling keeps the entered credentials available for correction.
    }
  }

  const unavailable = item?.available === false
  return (
    <Card>
      <CardHeader className="gap-2">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <CardTitle className="flex items-center gap-2">
            <Bot className="h-5 w-5" />
            {uiText("Telegram 通知")}
          </CardTitle>
          <Badge variant={item?.configured && item.enabled ? "default" : "secondary"}>
            {item?.configured
              ? item.enabled
                ? uiText("已启用")
                : uiText("已停用")
              : uiText("未配置")}
          </Badge>
        </div>
        {item?.botUsername && <p className="text-sm text-muted-foreground">@{item.botUsername}</p>}
      </CardHeader>
      <CardContent className="space-y-4">
        {unavailable && (
          <div className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
            {uiText("服务端尚未配置通知加密密钥。")}
          </div>
        )}
        <form className="grid gap-4 md:grid-cols-2" onSubmit={submit}>
          <Field label={uiText("机器人令牌")}>
            <PasswordInput
              value={botToken}
              onChange={(event) => setBotToken(event.target.value)}
              placeholder={item?.tokenSet ? uiText("留空以保留当前 Token") : "123456789:AA..."}
              disabled={unavailable}
              autoComplete="new-password"
            />
          </Field>
          <Field label={uiText("聊天 ID")}>
            <Input
              value={chatId}
              onChange={(event) => setChatId(event.target.value)}
              placeholder={uiText("-1001234567890 或 @channel")}
              disabled={unavailable}
              required
            />
          </Field>
          <div className="flex items-center gap-3 md:col-span-2">
            <Switch
              checked={enabled}
              onCheckedChange={setEnabled}
              disabled={unavailable || pending}
              aria-label={uiText("启用 Telegram 通知")}
            />
            <span className="text-sm">{uiText("启用通知渠道")}</span>
          </div>
          <div className="flex flex-wrap gap-2 md:col-span-2">
            <Button
              type="submit"
              disabled={
                unavailable || pending || !chatId.trim() || (!item?.tokenSet && !botToken.trim())
              }
            >
              {pending ? uiText("处理中...") : uiText("保存")}
            </Button>
            <Button
              type="button"
              variant="outline"
              disabled={unavailable || pending || !item?.configured}
              onClick={() => void onTest()}
            >
              <RefreshCcw className="h-4 w-4" />
              {uiText("发送测试")}
            </Button>
            {item?.configured && (
              <Button
                type="button"
                variant="ghost"
                className="text-destructive"
                disabled={pending}
                onClick={() => setConfirmOpen(true)}
              >
                <Trash2 className="h-4 w-4" />
                {uiText("删除")}
              </Button>
            )}
          </div>
        </form>
        {(item?.lastDeliveredAt || item?.lastError) && (
          <div className="border-t pt-3 text-xs text-muted-foreground">
            {item.lastDeliveredAt && (
              <div>
                {uiText("最近送达：{0}", [
                  new Date(item.lastDeliveredAt).toLocaleString(getInitialLanguage()),
                ])}
              </div>
            )}
            {item.lastError && (
              <div className="mt-1 break-words text-destructive">
                {uiText("最近错误：{0}", [uiText(errorMessage(item.lastError))])}
              </div>
            )}
          </div>
        )}
      </CardContent>
      <ConfirmDialog
        open={confirmOpen}
        title={uiText("删除 Telegram 配置？")}
        description={uiText("待发送的 Telegram 通知也会一并取消。")}
        confirmText={uiText("删除配置")}
        destructive
        onOpenChange={setConfirmOpen}
        onConfirm={() => {
          void onDelete()
          setConfirmOpen(false)
        }}
      />
    </Card>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  useUiLanguage()

  return (
    <div className="space-y-2">
      <Label>{uiText(label)}</Label>
      {children}
    </div>
  )
}
