import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { api, type BackupEncryptionConfiguration } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { validNewBackupPassword } from "@/lib/backup-password"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Label } from "@/components/ui/label"
import { PasswordInput } from "@/components/ui/password-input"

export function BackupEncryptionSettings({
  userId,
  editable,
}: {
  userId: string
  editable: boolean
}) {
  useLanguage()
  const client = useQueryClient()
  const configuration = useQuery({
    queryKey: ["admin", "backup-encryption", userId],
    queryFn: api.backupEncryptionConfiguration,
    enabled: !!userId,
    retry: false,
  })
  const [editor, setEditor] = React.useState<{
    snapshot: BackupEncryptionConfiguration
  } | null>(null)
  const [pending, setPending] = React.useState(false)
  const [error, setError] = React.useState("")
  const busy = React.useRef(false)
  const formId = React.useId()
  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!editor || busy.current) return
    const form = event.currentTarget
    const fields = new FormData(form)
    const payload = {
      revision: editor.snapshot.revision,
      mode: "password" as const,
      currentPassword: String(fields.get("currentPassword") || ""),
      password: String(fields.get("backupPassword") || ""),
      confirmPassword: String(fields.get("confirmPassword") || ""),
    }
    if (!validNewBackupPassword(payload.password) || payload.password !== payload.confirmPassword) {
      setError("备份密码须为 8 至 128 个字符，且两次输入必须相同")
      payload.currentPassword = payload.password = payload.confirmPassword = ""
      fields.delete("currentPassword")
      fields.delete("backupPassword")
      fields.delete("confirmPassword")
      return
    }
    busy.current = true
    setPending(true)
    setError("")
    form.reset()
    try {
      await api.saveBackupEncryptionConfiguration(payload)
      setEditor(null)
      await Promise.all([
        client.invalidateQueries({ queryKey: ["admin", "backup-encryption", userId] }),
        client.invalidateQueries({ queryKey: ["admin", "backup-capabilities", userId] }),
        client.invalidateQueries({ queryKey: ["admin", "backup-schedules", userId] }),
      ])
    } catch (cause) {
      setError(errorMessage(cause))
      void configuration.refetch()
    } finally {
      payload.currentPassword = payload.password = payload.confirmPassword = ""
      fields.delete("currentPassword")
      fields.delete("backupPassword")
      fields.delete("confirmPassword")
      busy.current = false
      setPending(false)
    }
  }
  return (
    <section className="space-y-3 rounded-lg border p-4" data-lanqin-i18n-ignore>
      <header className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <Label>{uiText("备份加密")}</Label>
          {configuration.data && (
            <Badge variant={configuration.data.passwordSet ? "secondary" : "outline"}>
              {uiText(configuration.data.passwordSet ? "备份密码已设置" : "尚未设置备份密码")}
            </Badge>
          )}
        </div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={!editable || !configuration.data || configuration.isError || pending}
          onClick={() => {
            setError("")
            setEditor({ snapshot: configuration.data! })
          }}
        >
          {uiText(configuration.data?.passwordSet ? "更改备份密码" : "设置备份密码")}
        </Button>
      </header>
      {configuration.isPending && <div role="status">{uiText("加载中...")}</div>}
      {configuration.isError && (
        <div role="alert" className="text-sm text-destructive">
          {uiText(errorMessage(configuration.error))}
          <Button type="button" variant="ghost" onClick={() => void configuration.refetch()}>
            {uiText("重试")}
          </Button>
        </div>
      )}
      {configuration.data && (
        <div
          role={configuration.data.passwordSet ? "note" : "alert"}
          className={configuration.data.passwordSet ? "text-sm" : "rounded-md border p-3 text-sm"}
        >
          {uiText(
            configuration.data.passwordSet
              ? "恢复时使用创建备份时的密码；请离线保存，丢失后无法找回"
              : "必须先由超级管理员设置备份密码，才能新建备份或启用计划；服务器不会自动生成备份密码"
          )}
        </div>
      )}
      <Dialog
        open={!!editor}
        onOpenChange={(open) => {
          if (!open && !pending) {
            setEditor(null)
            setError("")
          }
        }}
      >
        <DialogContent
          className="max-h-[90svh] overflow-y-auto"
          aria-describedby={undefined}
          data-lanqin-i18n-ignore
        >
          <DialogHeader>
            <DialogTitle>
              {uiText(editor?.snapshot.passwordSet ? "更改备份密码" : "设置备份密码")}
            </DialogTitle>
          </DialogHeader>
          <form className="space-y-4" autoComplete="off" onSubmit={submit}>
            {editor?.snapshot.passwordSet && (
              <div className="space-y-2">
                <Label htmlFor={`${formId}-current`}>{uiText("当前备份密码")}</Label>
                <PasswordInput
                  id={`${formId}-current`}
                  name="currentPassword"
                  required
                  maxLength={1024}
                  autoComplete="off"
                  disabled={pending}
                />
              </div>
            )}
            <>
              <div className="space-y-2">
                <Label htmlFor={`${formId}-new`}>{uiText("新的备份密码")}</Label>
                <PasswordInput
                  id={`${formId}-new`}
                  name="backupPassword"
                  required
                  autoComplete="new-password"
                  disabled={pending}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor={`${formId}-confirm`}>{uiText("确认新的备份密码")}</Label>
                <PasswordInput
                  id={`${formId}-confirm`}
                  name="confirmPassword"
                  required
                  autoComplete="new-password"
                  disabled={pending}
                />
              </div>
            </>
            <div role="note" className="rounded-md border p-3 text-sm">
              {uiText("为执行无人值守的计划，服务器会加密保存备份密码；云端只收到加密归档")}
            </div>
            <div role="note" className="rounded-md border p-3 text-sm">
              {uiText(
                "修改只影响之后的新备份，不会重新加密历史文件；旧备份仍需原密码或原私钥。已有计划需重新确认"
              )}
            </div>
            {error && (
              <div role="alert" className="text-sm text-destructive">
                {uiText(error)}
              </div>
            )}
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                disabled={pending}
                onClick={() => {
                  setEditor(null)
                  setError("")
                }}
              >
                {uiText("取消")}
              </Button>
              <Button type="submit" disabled={pending || !editor}>
                {uiText(pending ? "处理中..." : "保存加密设置")}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </section>
  )
}
