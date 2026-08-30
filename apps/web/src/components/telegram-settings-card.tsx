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
  onSave: (payload: { botToken?: string; chatId: string; enabled: boolean }) => Promise<TelegramSettings>
  onTest: () => Promise<void>
  onDelete: () => Promise<void>
}

export function TelegramSettingsCard({ item, pending, onSave, onTest, onDelete }: TelegramSettingsCardProps) {
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
          <CardTitle className="flex items-center gap-2"><Bot className="h-5 w-5" />Telegram 通知</CardTitle>
          <Badge variant={item?.configured && item.enabled ? "default" : "secondary"}>{item?.configured ? (item.enabled ? "已启用" : "已停用") : "未配置"}</Badge>
        </div>
        {item?.botUsername && <p className="text-sm text-muted-foreground">@{item.botUsername}</p>}
      </CardHeader>
      <CardContent className="space-y-4">
        {unavailable && <div className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">服务端尚未配置通知加密密钥。</div>}
        <form className="grid gap-4 md:grid-cols-2" onSubmit={submit}>
          <Field label="Bot Token"><PasswordInput value={botToken} onChange={(event) => setBotToken(event.target.value)} placeholder={item?.tokenSet ? "留空以保留当前 Token" : "123456789:AA..."} disabled={unavailable} autoComplete="new-password" /></Field>
          <Field label="Chat ID"><Input value={chatId} onChange={(event) => setChatId(event.target.value)} placeholder="-1001234567890 或 @channel" disabled={unavailable} required /></Field>
          <div className="flex items-center gap-3 md:col-span-2">
            <Switch checked={enabled} onCheckedChange={setEnabled} disabled={unavailable || pending} aria-label="启用 Telegram 通知" />
            <span className="text-sm">启用通知渠道</span>
          </div>
          <div className="flex flex-wrap gap-2 md:col-span-2">
            <Button type="submit" disabled={unavailable || pending || !chatId.trim() || (!item?.tokenSet && !botToken.trim())}>{pending ? "处理中..." : "保存"}</Button>
            <Button type="button" variant="outline" disabled={unavailable || pending || !item?.configured} onClick={() => void onTest()}><RefreshCcw className="h-4 w-4" />发送测试</Button>
            {item?.configured && <Button type="button" variant="ghost" className="text-destructive" disabled={pending} onClick={() => setConfirmOpen(true)}><Trash2 className="h-4 w-4" />删除</Button>}
          </div>
        </form>
        {(item?.lastDeliveredAt || item?.lastError) && <div className="border-t pt-3 text-xs text-muted-foreground">
          {item.lastDeliveredAt && <div>最近送达：{new Date(item.lastDeliveredAt).toLocaleString()}</div>}
          {item.lastError && <div className="mt-1 break-words text-destructive">最近错误：{item.lastError}</div>}
        </div>}
      </CardContent>
      <ConfirmDialog open={confirmOpen} title="删除 Telegram 配置？" description="待发送的 Telegram 通知也会一并取消。" confirmText="删除配置" destructive onOpenChange={setConfirmOpen} onConfirm={() => { void onDelete(); setConfirmOpen(false) }} />
    </Card>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return <div className="space-y-2"><Label>{label}</Label>{children}</div>
}
